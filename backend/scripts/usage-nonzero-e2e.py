#!/usr/bin/env python3
"""Verify nonzero usage and empty-response billing through real account scheduling."""
import argparse
import importlib.util
import json
import math
import time
from pathlib import Path

spec = importlib.util.spec_from_file_location('gateway_e2e', Path(__file__).with_name('cache-gateway-e2e.py'))
g = importlib.util.module_from_spec(spec)
spec.loader.exec_module(g)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--base', default='http://127.0.0.1:48788')
    parser.add_argument('--mock-port', type=int, default=48791)
    parser.add_argument('--report', default='/tmp/usage-nonzero-e2e.json')
    parser.add_argument('--filter', default='')
    parser.add_argument('--turns',type=int,default=2)
    parser.add_argument('--wire',choices=['chat','responses'])
    parser.add_argument('--variants', default='zero-factor,tiny-factor,zero-input,zero-output-text,zero-output-tool,zero-both-text,empty,whitespace')
    args = parser.parse_args()
    runner = g.Runner(args)
    errors = []
    try:
        runner.bootstrap()
        initial_balance=runner.api('GET','/admin/users/'+str(runner.user_id))['balance']
        for protocol in ['anthropic', 'gemini']:
            for kind in g.KINDS:
                if args.filter and protocol+'/'+kind not in args.filter.split(','):
                    continue
                for variant in args.variants.split(','):
                    if variant=='raw' and kind!='prefix_tool_plus':
                        continue
                    raw = {'input':10000, 'output':1200, 'read':2000, 'write':500 if protocol=='anthropic' else 0}
                    if variant in ['zero-input','zero-both-text']:
                        raw['input'] = 0
                    if variant not in ['zero-factor','tiny-factor','zero-input','cached-only']:
                        raw['output'] = 0
                    if variant in ['zero-both-text','missing-usage']:
                        raw['input']=0
                        raw['read']=raw['write']=0
                    patch = {}
                    if variant in ['zero-factor','tiny-factor']:
                        factor = 0 if variant=='zero-factor' else .000001
                        if kind=='usage_multiplier':
                            patch = {'usage_multiplier':{field:{'multiplier':factor,'threshold_tokens':0} for field in ['input','output','cache_read','cache_creation']}}
                        elif kind=='prefix_tool_plus':
                            patch = {'tool_plus':{'shaping':{field:{'multiplier':factor} for field in ['input','output','cache_read','cache_creation']}}}
                        else:
                            patch = {'uncached_input_min_tokens':0,'uncached_input_max_tokens':0}
                    if variant=='cached-only':raw['input']=0
                    if variant in ['skip','raw','disabled-policy']:
                        patch={'usage_multiplier':{'output':{'multiplier':100}}}
                        if variant=='skip':patch['usage']={'skip_non_stream_usage_projection':True}
                        if variant=='raw':patch['tool_plus']={'non_stream':{'mode':'raw'},'small_request':{'enabled':True,'max_input_tokens':100000,'mode':'raw'}}
                    if variant=='cap-one':patch={'limits':{'max_input_tokens':1,'max_output_tokens':1}}
                    if variant=='estimated-double':patch={'usage_multiplier':{'output':{'multiplier':2}},'tool_plus':{'shaping':{'output':{'multiplier':2}}}}
                    mock={'missing-usage':'missing_usage','terminal-no-usage':'terminal_no_usage','interrupted-text':'incomplete','interrupted-empty':'incomplete'}.get(variant,'normal')
                    case = runner.setup(protocol,kind,variant,patch=patch,raw=raw,mock=mock,wire=args.wire if protocol=='anthropic' else None)
                    if variant=='disabled-policy':runner.api('POST','/admin/cache-strategies/'+str(case['strategy']['id'])+'/disable',{})
                    label = f'非零验收 · {protocol} · {kind} · {variant}'
                    runner.api('PUT','/admin/accounts/'+str(case['account']['id']),{'name':label})
                    runner.api('PUT','/admin/groups/'+str(case['group']['id']),{'name':label})
                    with g.LOCK:
                        if variant in ['empty','whitespace','interrupted-empty']:
                            g.CASES[case['id']]['response_text'] = '' if variant=='empty' else '  \n  '
                        if variant=='zero-output-tool':
                            g.CASES[case['id']]['tool_output'] = True
                    records = [runner.forward(case,stream,case['id']+str(stream),turn,body=runner.body(case,stream,case['id']+str(stream),turn,maxout=1 if variant in ['skip','raw','disabled-policy','cap-one'] else 20000)) for stream in ([True] if variant.startswith('interrupted') else [False,True]) for turn in range(args.turns)]
                    for _ in range(40):
                        rows = runner.usage_rows(case)
                        if len(rows)==len(records):
                            break
                        time.sleep(.15)
                    assert len(rows)==len(records),(case['id'],'missing logs',len(rows))
                    by_request={row['request_id'].removeprefix('client:'):row for row in rows}
                    for rec in records:
                        row=by_request[rec['request_id']]
                        assert row['account_id']==case['account']['id'] and row['group_id']==case['group']['id'],('wrong scheduler',case['id'])
                        assert row['cache_strategy_id']==case['strategy']['id'],('wrong strategy',case['id'])
                        if not variant.startswith('interrupted'):
                            for field,wire in [('input_tokens','input'),('output_tokens','output'),('cache_read_tokens','read'),('cache_creation_tokens','write')]:
                                assert row[field]==rec['wire'][wire],(case['id'],'wire/db mismatch',field,row[field],rec['wire'])
                        assert row['input_tokens']>=1,(case['id'],'input zero')
                        empty=variant in ['empty','whitespace','interrupted-empty']
                        if empty:
                            assert row['output_tokens']==0 and row['total_cost']==0 and row['actual_cost']==0,(case['id'],'empty reply billed',row)
                        else:
                            assert row['output_tokens']>=1 and row['raw_output_tokens']>=1,(case['id'],'output zero')
                        if variant=='estimated-double' and kind in ['usage_multiplier','prefix_tool_plus']:
                            assert row['output_tokens']==row['raw_output_tokens']*2,(case['id'],'estimate bypassed shaping',row['output_tokens'],row['raw_output_tokens'])
                        assert row['raw_input_tokens']>=1,(case['id'],'raw input repair missing')
                        if variant in ['skip','raw','disabled-policy','cap-one']:assert row['output_tokens']==1,(case['id'],'repaired output exceeds request cap',row['output_tokens'])
                        if variant=='cap-one' and kind!='disabled':
                            assert row['input_tokens']+row['cache_read_tokens']+row['cache_creation_tokens']==1,(case['id'],'input budget violated')
                        if variant in ['zero-input','cached-only']:
                            assert row['raw_input_tokens']==1,(case['id'],'cache-backed input floor changed')
                            assert row['raw_input_tokens']+row['raw_cache_read_tokens']+row['raw_cache_creation_tokens']==sum(raw[k] for k in ['input','read','write']),(case['id'],'cache-backed floor inflated total')
                        if raw['output']:
                            assert row['raw_output_tokens']==raw['output'],(case['id'],'raw nonzero changed')
                        prices={'input':3e-6,'output':15e-6,'read':.3e-6,'write5':3.75e-6,'write1':6e-6} if protocol=='anthropic' else {'input':1.25e-6,'output':10e-6,'read':.125e-6,'write5':0,'write1':0}
                        original_cost=raw['input']*prices['input']+raw['output']*prices['output']+raw['read']*prices['read']
                        if protocol=='anthropic':
                            short=raw['write']*3//5
                            original_cost+=short*prices['write5']+(raw['write']-short)*prices['write1']
                        assert math.isclose(row['account_stats_cost'],original_cost,abs_tol=1e-7),(case['id'],'upstream cost includes estimate or shaping',row['account_stats_cost'],original_cost)
                        raw_expected=row['raw_input_tokens']*prices['input']+row['raw_output_tokens']*prices['output']+row['raw_cache_read_tokens']*prices['read']
                        if protocol=='anthropic':
                            original_short=raw['write']*3//5
                            original_long=raw['write']-original_short
                            raw_expected+=row['raw_cache_creation_tokens']*prices['write5']+original_long*(prices['write1']-prices['write5'])
                        assert math.isclose(row['raw_total_cost'],raw_expected,abs_tol=1e-7),(case['id'],'repaired raw cost mismatch',row['raw_total_cost'],raw_expected)
                        if not empty:
                            expected=row['input_tokens']*prices['input']+row['output_tokens']*prices['output']+row['cache_read_tokens']*prices['read']
                            if protocol=='anthropic':
                                expected+=(row['cache_creation_tokens']-row['cache_creation_1h_tokens'])*prices['write5']+row['cache_creation_1h_tokens']*prices['write1']
                            assert math.isclose(row['total_cost'],expected,abs_tol=1e-7),(case['id'],'customer cost mismatch',row['total_cost'],expected)
                        rec['db']={field:row.get(field) for field in ['id','input_tokens','output_tokens','cache_read_tokens','cache_creation_tokens','raw_input_tokens','raw_output_tokens','raw_cache_read_tokens','raw_cache_creation_tokens','total_cost','actual_cost','raw_total_cost','account_stats_cost']}
                    print('PASS',protocol,kind,variant,'wire/db nonzero, empty billing, upstream cost',flush=True)
        final_balance=runner.api('GET','/admin/users/'+str(runner.user_id))['balance']
        expected_debit=sum(rec['db']['actual_cost'] for rec in runner.results)
        assert math.isclose(initial_balance-final_balance,expected_debit,abs_tol=1e-6),('balance mismatch',initial_balance-final_balance,expected_debit)
        runner.checkpoints.append({'case':'balance-debit','balance_change':initial_balance-final_balance,'expected_debit':expected_debit,'passed':True})
    except Exception as exc:
        errors.append(repr(exc))
        print('FAIL',repr(exc),flush=True)
    finally:
        runner.finish(errors)


if __name__=='__main__':
    main()
