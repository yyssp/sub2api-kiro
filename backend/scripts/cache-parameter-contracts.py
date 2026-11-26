#!/usr/bin/env python3
"""API omission/null contracts and cross-field rejection for every strategy."""
import argparse,copy,importlib.util,json
from pathlib import Path
spec=importlib.util.spec_from_file_location('parameter_audit',Path(__file__).with_name('cache-parameter-e2e.py'));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
g=m.g

def main():
 p=argparse.ArgumentParser();p.add_argument('--base',default='http://127.0.0.1:48788');p.add_argument('--mock-port',type=int,default=48795);p.add_argument('--report',default='/tmp/cache-parameter-contracts.json');a=p.parse_args();r=g.Runner(a);records=[];errors=[]
 try:
  r.bootstrap()
  for protocol in ['anthropic','gemini']:
   for kind in g.KINDS:
    default=copy.deepcopy(m.DEFAULTS[protocol+'/'+kind]);default.setdefault('model_min_cacheable_overrides',{})
    item=r.api('POST','/admin/cache-strategies',{'name':r.prefix+'_'+protocol+'_'+kind,'config':default,'enabled':True});r.resources.append(('strategy',item['id']))
    def trial(label,cfg,expected):
     nonlocal item
     code,text,_=r.request('PUT','/api/v1/admin/cache-strategies/'+str(item['id']),{'name':item['name'],'config':cfg,'expected_revision':item['revision']},token=r.admin)
     entry={'protocol':protocol,'kind':kind,'trial':label,'status':code,'passed':code==200 if expected else 400<=code<500};records.append(entry)
     if code==200:item=json.loads(text)['data']
     if not entry['passed']:errors.append({**entry,'detail':text[:300]});print('FAIL',entry,flush=True)
    for path,value in m.flatten(default):
     for mode in ['omitted','null']:
      cfg=copy.deepcopy(default);node=cfg;keys=path.split('.')
      for key in keys[:-1]:node=node[key]
      if mode=='omitted':node.pop(keys[-1],None)
      else:node[keys[-1]]=None
      trial(path+':'+mode,cfg,not(kind=='disabled' and path=='kind'))
    for label,patch in [
     ('all-sources-off',{'cache_system':False,'cache_tools':False,'cache_history':False,'cache_current_user_stable_prefix':False}),
     ('reported-inverted',{'reported_input_min_tokens':200,'reported_input_max_tokens':100}),
     ('uncached-inverted',{'uncached_input_min_tokens':200,'uncached_input_max_tokens':100}),
     ('stable-inverted',{'stable_segment':{'reported_input_min_tokens':200,'reported_input_max_tokens':100}}),
     ('ttl-inverted',{'default_ttl_seconds':300,'hour_ttl_seconds':1}),
     ('jitter-inverted',{'cap_jitter_min_tokens':200,'cap_jitter_max_tokens':100}),
     ('creation-inverted',{'creation_control':{'enabled':True,'min_creation_delta_tokens':200,'max_creation_tokens_per_event':100}}),
     ('sample-max-zero',{'usage':{'enabled':True,'output':{'mode':'sample_max','max_tokens':0}}}),
     ('sample-target-zero',{'usage':{'enabled':True,'output':{'mode':'sample_target','target_tokens':0}}}),
     ('plus-inverted',{'tool_plus':{'shaping':{'output':{'min_tokens':200,'max_tokens':100}}}}),
    ]:
     cfg=copy.deepcopy(default);g.deep(cfg,patch);trial(label,cfg,label=='all-sources-off' and kind in ['disabled','usage_multiplier'])
    for alias,canonical in [('no_cache','disabled'),('current_high_cache','prefix'),('prefix_tool','tool_aware'),('kiro_rs_tool','tool_aware'),('kiro_rs_tool_plus','prefix_tool_plus')]:
     cfg=copy.deepcopy(m.DEFAULTS[protocol+'/'+canonical]);cfg['kind']=alias;trial('alias:'+alias,cfg,True)
     if item['config']['kind']!=canonical:errors.append({'alias':alias,'expected':canonical,'actual':item['config']['kind']})
    # Default-only construction through the real API.
    trial('minimal-defaults',{'protocol':protocol,'kind':kind},True)
    expected=m.DEFAULTS[protocol+'/'+kind]
    if item['config']!=expected:errors.append({'protocol':protocol,'kind':kind,'error':'minimal defaults drift'})
    Path(a.report).write_text(json.dumps({'run':r.prefix,'records':records,'errors':errors},ensure_ascii=False,indent=2));print('PASS contracts',protocol,kind,len(records),'trials',len(errors),'errors',flush=True)
 except Exception as e:errors.append({'fatal':repr(e)});print('FATAL',repr(e),flush=True)
 finally:
  cleanup=r.cleanup();r.server.shutdown();Path(a.report).write_text(json.dumps({'run':r.prefix,'records':records,'errors':errors,'cleanup_errors':cleanup},ensure_ascii=False,indent=2));print('FINISH',len(records),'trials',len(errors),'errors',flush=True)
 if errors or cleanup:raise SystemExit(1)
if __name__=='__main__':main()
