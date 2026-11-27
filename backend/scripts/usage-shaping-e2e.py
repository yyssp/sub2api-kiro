#!/usr/bin/env python3
"""Verify aggregate costs, tokens and requests through the running mock gateway."""
import argparse
import importlib.util
import json
import math
from pathlib import Path
import time

spec = importlib.util.spec_from_file_location('gateway_e2e', Path(__file__).with_name('cache-gateway-e2e.py'))
g = importlib.util.module_from_spec(spec)
spec.loader.exec_module(g)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--base', default='http://127.0.0.1:48788')
    parser.add_argument('--mock-port', type=int, default=48791)
    parser.add_argument('--report', default='/tmp/usage-shaping-live-e2e.json')
    args = parser.parse_args()
    runner = g.Runner(args)
    errors = []
    expected = {}
    try:
        runner.bootstrap()
        for protocol in ['anthropic', 'gemini']:
            for kind in g.KINDS:
                variants = ['default'] if kind != 'usage_multiplier' else ['grow', 'shrink', 'zero', 'threshold']
                for variant in variants:
                    raw = {'input': 10000, 'output': 1200, 'read': 2000, 'write': 500 if protocol == 'anthropic' else 0}
                    patch = None
                    if kind == 'usage_multiplier':
                        factor = {'grow': 1.5, 'shrink': .5, 'zero': 0, 'threshold': 2}[variant]
                        patch = {'usage_multiplier': {bucket: {'multiplier': factor, 'threshold_tokens': (raw[field] if variant == 'threshold' else 0)} for bucket, field in [('input','input'),('output','output'),('cache_read','read'),('cache_creation','write')]}}
                    case = runner.setup(protocol, kind, variant, patch=patch, raw=raw)
                    label = f'整形验证 · {protocol} · {kind} · {variant}'
                    runner.api('PUT', '/admin/accounts/'+str(case['account']['id']), {'name': label})
                    runner.api('PUT', '/admin/groups/'+str(case['group']['id']), {'name': label})
                    records = [runner.forward(case, stream, case['id']+str(stream), turn) for stream in [False,True] for turn in range(2)]
                    rows = runner.verify_db(case,records)
                    metrics = {'requests':len(rows),'comparable_requests':len(rows),'before_tokens':0,'after_tokens':0,'before_cost':0.,'after_cost':0.}
                    for row in rows:
                        metrics['before_tokens'] += sum(row['raw_'+key] for key in ['input_tokens','output_tokens','cache_read_tokens','cache_creation_tokens'])
                        metrics['after_tokens'] += sum(row[key] for key in ['input_tokens','output_tokens','cache_read_tokens','cache_creation_tokens'])
                        metrics['before_cost'] += row['raw_total_cost']
                        metrics['after_cost'] += row['total_cost']
                    if variant == 'grow': assert metrics['after_cost'] > metrics['before_cost']
                    if variant == 'shrink': assert metrics['after_cost'] < metrics['before_cost']
                    if variant == 'zero':
                        assert all(row['input_tokens']==1 and row['output_tokens']==1 and row['cache_read_tokens']==0 and row['cache_creation_tokens']==0 for row in rows)
                        expected_floor=4*(3e-6+15e-6 if protocol=='anthropic' else 1.25e-6+10e-6)
                        assert math.isclose(metrics['after_cost'],expected_floor,abs_tol=1e-7)
                    if variant == 'threshold': assert abs(metrics['after_cost']-metrics['before_cost']) < 1e-7
                    expected[(case['account']['id'],case['group']['id'])] = metrics
                    print('PASS', protocol, kind, variant, 'raw/final costs and tokens', flush=True)
        for dimension in ['account','group']:
            for granularity in ['hour','day']:
                result = runner.api('GET',f'/admin/usage/shaping-stats?dimension={dimension}&granularity={granularity}&timezone=Asia%2FShanghai')
                target = {key[0 if dimension=='account' else 1]:value for key,value in expected.items()}
                for row in result['breakdown']:
                    if row['id'] in target:
                        for field,value in target.pop(row['id']).items():
                            assert math.isclose(row[field],value,abs_tol=1e-7),(dimension,granularity,field,row[field],value)
                assert not target,(dimension,granularity,'missing dimensions',target)
                for field in ['requests','comparable_requests','before_tokens','after_tokens','before_cost','after_cost']:
                    total = sum(row[field] for row in result['breakdown'])
                    series = sum(row[field] for row in result['series'])
                    assert math.isclose(result['summary'][field],total,abs_tol=1e-7),(field,'summary mismatch')
                    assert math.isclose(series,total,abs_tol=1e-7),(field,'series mismatch')
                assert all(row['comparable_requests']<=row['requests'] for row in result['breakdown'])
                runner.checkpoints.append({'dimension':dimension,'granularity':granularity,'verified_entities':len(expected),'summary':result['summary']})
                print('PASS',dimension,granularity,'summary = breakdown = series',flush=True)
        for query in ['dimension=bad','granularity=minute','timezone=invalid','start_date=2026-11-28&end_date=2026-11-27','start_date=2026-11-27']:
            status,_,_=runner.request('GET','/api/v1/admin/usage/shaping-stats?'+query,token=runner.admin)
            assert status==400,(query,status)
        print('PASS invalid filters rejected',flush=True)
    except Exception as exc:
        errors.append(repr(exc))
        print('FAIL',repr(exc),flush=True)
    finally:
        runner.finish(errors)

if __name__ == '__main__':
    main()
