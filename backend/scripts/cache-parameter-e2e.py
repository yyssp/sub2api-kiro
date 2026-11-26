#!/usr/bin/env python3
"""Field-by-field API and live gateway audit of every cache strategy config.
The coverage gate fails if a discovered JSON leaf has no executed mutation.
"""
import argparse
import copy
import importlib.util
import json
import math
import time
from pathlib import Path

SPEC = importlib.util.spec_from_file_location('gateway_e2e', Path(__file__).with_name('cache-gateway-e2e.py'))
g = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(g)
ROOT = Path(__file__).resolve().parents[2]
DEFAULTS = json.loads((ROOT / 'frontend/src/views/admin/cacheStrategyDefaults.json').read_text())
ENUMS = {
 'protocol':['anthropic','gemini'], 'kind':g.KINDS,
 'ratio_mode':['uniform','independent'], 'breakpoint_mode':['client_only','auto','hybrid'],
 'dynamic_content_mode':['exclude','allow'], 'scope_mode':['group_session','group_account_session'],
 'forced_ttl_tier':['','5m','1h'],
 'tool_plus.non_stream.mode':['inherit','custom','raw'],
 'tool_plus.small_request.mode':['inherit','custom','raw'],
 'tool_plus.suppressed_creation_target':['input','cache_read','drop'],
}
for bucket in ['input','output','cache_read','cache_creation']:
 ENUMS['usage.'+bucket+'.mode']=['raw','preserve','sample_max','sample_target']
FLOAT_MAX = {'coverage_ratio':1,'usage_ratio':1,'read_ratio':1,'creation_ratio':1,'token_scale':3,
 'hit_shaping.target_hit_ratio':.99,'hit_shaping.ratio_jitter':.2,'hit_shaping.min_cost_ratio':5,
 'hit_shaping.max_read_multiplier':50,'hit_shaping.min_creation_ratio':5}


def flatten(value, prefix=''):
 for key, child in value.items():
  path=prefix+'.'+key if prefix else key
  if isinstance(child,dict) and child: yield from flatten(child,path)
  else: yield path,child


def put(obj,path,value):
 keys=path.split('.')
 for key in keys[:-1]:obj=obj.setdefault(key,{})
 obj[keys[-1]]=copy.deepcopy(value)


def get(obj,path):
 for key in path.split('.'):obj=obj[key]
 return obj


def values(path,default,protocol):
 if path in ENUMS:return ENUMS[path]
 if isinstance(default,bool):return [False,True]
 if path=='model_min_cacheable_overrides':
  model='claude-sonnet-4-5' if protocol=='anthropic' else 'gemini-2.5-pro'
  return [{},{model:1},{model:1000000}]
 if path=='token_scale':return [1,3]
 if path=='state_ttl_seconds':return [0,1,300]
 if path in ['default_ttl_seconds','hour_ttl_seconds']:return [1,3600]
 if path.endswith('.normal_max_multiplier'):return [1,2]
 if path.endswith('.multiplier'):return [0,.5,2,1000]
 if path.endswith('.jitter_ratio'):return [0,.2,1]
 if path in FLOAT_MAX:return [0,FLOAT_MAX[path]]
 if path.endswith('.min_cost_ratio'):return [0,2,100]
 if isinstance(default,(float,int)):return [0,37]
 raise AssertionError(('unclassified config leaf',path,default))


def field_config(default,path,value):
 cfg=copy.deepcopy(default)
 if path=='kind' and value!='disabled':cfg['cache_system']=True
 # Activate dependent controls, independently of the changed leaf.
 if path.startswith('usage.'):
  for bucket in ['input','output','cache_read','cache_creation']:
   cfg['usage'][bucket]['max_tokens']=500
   cfg['usage'][bucket]['target_tokens']=500
  cfg['usage']['enabled']=True
  if '.target_tokens' in path or '.normal_max_multiplier' in path:
   cfg['usage'][path.split('.')[1]]['mode']='sample_target'
  if '.max_tokens' in path:cfg['usage'][path.split('.')[1]]['mode']='sample_max'
  if path.startswith('usage.final_'):
   cfg['usage']['final_output_guard_enabled']=True
   for bucket in ['cache_read','cache_creation','output']:
    cfg['usage']['final_'+bucket+'_max_tokens']=500
    cfg['usage']['final_'+bucket+'_jitter_min_tokens']=0
    cfg['usage']['final_'+bucket+'_jitter_max_tokens']=0
  if path.startswith('usage.output_uplift'):
   cfg['usage']['final_output_guard_enabled']=True
   cfg['usage']['output_uplift_enabled']=True
   cfg['usage']['output_uplift_min_tokens']=1000
   cfg['usage']['output_uplift_percent']=50
 if path.startswith('tool_plus.non_stream.profile.'):
  cfg['tool_plus']['non_stream']['mode']='custom'
 if path.startswith('tool_plus.small_request.'):
  cfg['tool_plus']['small_request']['enabled']=True
  cfg['tool_plus']['small_request']['max_input_tokens']=4000000
  cfg['tool_plus']['small_request']['mode']='custom'
 if path.startswith('hit_shaping.'):
  cfg['hit_shaping']['enabled']=True
  cfg['cold_request_guard']=False
 if path.startswith('stable_segment.'):
  cfg['cold_request_guard']=False
  cfg['stable_segment']['min_total_input_tokens']=0
  cfg['stable_segment']['min_segment_tokens']=0
 if path=='read_ratio' or path=='creation_ratio':cfg['ratio_mode']='independent'
 if path=='current_user_stable_prefix_max_tokens':cfg['cache_current_user_stable_prefix']=True
 if path.startswith('creation_control.'):
  cfg['creation_control']['enabled']=True
  cfg['creation_control']['min_creation_delta_tokens']=0
  cfg['creation_control']['max_creation_tokens_per_event']=100000
 if path=='cap_jitter_min_tokens' or path=='cap_jitter_max_tokens':
  cfg['reported_input_max_tokens']=5000;cfg['cap_jitter_min_tokens']=0;cfg['cap_jitter_max_tokens']=100
 if path=='hour_ttl_seconds':cfg['default_ttl_seconds']=1
 if path.endswith('.min_tokens'):
  put(cfg,path.rsplit('.',1)[0]+'.max_tokens',0)
 if path.endswith('reported_input_min_tokens'):
  put(cfg,path.replace('_min_tokens','_max_tokens'),0)
 if path=='uncached_input_min_tokens':cfg['uncached_input_max_tokens']=0
 put(cfg,path,value)
 if path=='cap_jitter_min_tokens' and value>0:cfg['cap_jitter_max_tokens']=value
 if path=='creation_control.min_creation_delta_tokens' and value>100000:cfg['creation_control']['max_creation_tokens_per_event']=0
 # Zero sample limits have documented invalid semantics: retain the mode and test rejection separately.
 if path.endswith('.max_tokens') and path.startswith('usage.') and value==0:
  cfg['usage'][path.split('.')[1]]['mode']='raw'
 if path.endswith('.target_tokens') and value==0:
  cfg['usage'][path.split('.')[1]]['mode']='raw'
 if path=='cap_jitter_max_tokens' and value>0:cfg['cap_jitter_min_tokens']=0
 if path=='reported_input_max_tokens' and value>0:cfg['reported_input_min_tokens']=0
 if path=='uncached_input_max_tokens' and value>0:cfg['uncached_input_min_tokens']=0
 if path=='stable_segment.reported_input_max_tokens' and value>0:cfg['stable_segment']['reported_input_min_tokens']=0
 return cfg


def expected_normalized(cfg):
 c=copy.deepcopy(cfg)
 if c['kind']=='disabled':c=copy.deepcopy(DEFAULTS[c['protocol']+'/disabled'])
 if c['kind']!='disabled':c['usage']['preserve_upstream_cache_usage']=c['preserve_upstream_cache_usage']
 if c['ratio_mode']=='uniform':c['read_ratio']=c['creation_ratio']=c['usage_ratio']
 if not c['cache_current_user_stable_prefix']:c['current_user_stable_prefix_max_tokens']=0
 for bucket in ['cache_read','cache_creation','output']:
  stem='final_'+bucket;cap=c['usage'][stem+'_max_tokens'];lo=c['usage'][stem+'_jitter_min_tokens'];hi=c['usage'][stem+'_jitter_max_tokens']
  if cap<=1 or hi==0:lo=hi=0
  else:
   if hi<lo:lo,hi=hi,lo
   if hi>cap-1:lo=math.floor(lo*(cap-1)/hi);hi=cap-1
   if lo>=hi:lo=hi//2
  c['usage'][stem+'_jitter_min_tokens']=lo;c['usage'][stem+'_jitter_max_tokens']=hi
 c['usage']['output_uplift_percent']=min(c['usage']['output_uplift_percent'],200)
 if c['protocol']=='gemini':
  c['forced_ttl_tier']='';c['creation_ratio']=0
  c['creation_control']={key:False if key=='enabled' else 0 for key in c['creation_control']}
  c['usage']['cache_creation']={'mode':'preserve','max_tokens':0,'target_tokens':0,'normal_max_multiplier':1.1,'move_delta_to_cache_read':False}
  for key in ['final_cache_creation_max_tokens','final_cache_creation_jitter_min_tokens','final_cache_creation_jitter_max_tokens']:c['usage'][key]=0
  c['hit_shaping']['min_cache_creation_tokens']=c['hit_shaping']['min_creation_ratio']=0
 return c


def limits(path,default):
 if path in ENUMS:return [('enum-invalid','__invalid__')]
 if isinstance(default,bool):return [('type-invalid','bad-bool')]
 if isinstance(default,dict):return [('negative',{'test-model':-1}),('overflow',{'test-model':4000001})]
 if isinstance(default,(float,int)):
  upper=FLOAT_MAX.get(path,4000000)
  if path.endswith('.multiplier') or path.endswith('.normal_max_multiplier'):upper=1000
  if path.endswith('.jitter_ratio'):upper=1
  if path.endswith('.min_cost_ratio') and path.startswith('tool_plus.'):upper=100
  if path=='estimated_bytes_limit':upper=16<<30
  if path=='state_ttl_seconds':upper=300
  if path in ['default_ttl_seconds','hour_ttl_seconds']:upper=3600
  return [('negative',-1),('overflow',upper+1)]
 return []

def upper_value(path,default,protocol):
 if isinstance(default,(str,bool)):return None
 if isinstance(default,dict):return {'claude-sonnet-4-5' if protocol=='anthropic' else 'gemini-2.5-pro':4000000}
 upper=FLOAT_MAX.get(path,4000000)
 if path.endswith('.multiplier') or path.endswith('.normal_max_multiplier'):upper=1000
 if path.endswith('.jitter_ratio'):upper=1
 if path.endswith('.min_cost_ratio') and path.startswith('tool_plus.'):upper=100
 if path=='estimated_bytes_limit':upper=16<<30
 if path=='state_ttl_seconds':upper=300
 if path in ['default_ttl_seconds','hour_ttl_seconds']:upper=3600
 return upper


class Audit:
 def __init__(self,args):
  self.args=args;self.runner=g.Runner(args);self.coverage=[];self.errors=[];self.started=time.time()
 def save(self):
  d={'run':self.runner.prefix,'elapsed_seconds':round(time.time()-self.started,2),'coverage':self.coverage,'errors':self.errors,'success_generations':len(self.runner.results),'http_requests':len(self.runner.http)}
  Path(self.args.report).write_text(json.dumps(d,ensure_ascii=False,indent=2))
 def set_config(self,case,cfg):
  result=self.runner.api('PUT','/admin/cache-strategies/'+str(case['strategy']['id']),{'name':case['name'],'config':cfg,'expected_revision':case['strategy']['revision']})
  case['strategy']=result;case['config']=result['config']
  return result['config']
 def verify(self,case,records):
  case['verify_subset_ids']=[r['request_id'] for r in records]
  return self.runner.verify_db(case,records)
 def matrix(self):
  r=self.runner
  for protocol in ['anthropic','gemini']:
   for kind in g.KINDS:
    if self.args.filter and protocol+'/'+kind not in self.args.filter.split(','):continue
    default=copy.deepcopy(DEFAULTS[protocol+'/'+kind]);default.setdefault('model_min_cacheable_overrides',{})
    leaves=dict(flatten(default));case=r.setup(protocol,kind,'parameters',raw={'input':10000,'read':0,'write':0,'output':1200})
    for index,(path,initial) in enumerate(leaves.items()):
     if self.args.paths and not any(path.startswith(p) for p in self.args.paths.split(',')):continue
     candidates=values(path,initial,protocol)
     if self.args.upper_only:
      upper=upper_value(path,initial,protocol)
      if upper is None:continue
      candidates=[upper]
     entry={'protocol':protocol,'strategy':kind,'path':path,'default':initial,'trials':[],'passed':True}
     self.coverage.append(entry)
     for value in candidates:
      trial={'value':value}
      entry['trials'].append(trial)
      try:
       cfg=field_config(default,path,value);saved=self.set_config(case,cfg)
       expected=expected_normalized(cfg)
       # omitted empty maps are semantically empty, not missing coverage.
       expected.pop('model_min_cacheable_overrides',None) if not expected.get('model_min_cacheable_overrides') else None
       actual=copy.deepcopy(saved)
       actual.pop('model_min_cacheable_overrides',None) if not actual.get('model_min_cacheable_overrides') else None
       g.require(actual==expected,('normalization',[(p,v,dict(flatten(actual)).get(p)) for p,v in flatten(expected) if dict(flatten(actual)).get(p)!=v]))
       trial['stored_value']=get(saved,path) if path!='model_min_cacheable_overrides' else saved.get(path,{})
       session=case['id']+'-'+str(index)+'-'+str(len(entry['trials']));records=[]
       for turn,stream in enumerate([False,True]):
        rec=r.forward(case,stream,session,turn);records.append(rec)
        g.common_check(case,rec,turn)
        if saved['kind']=='disabled':g.require(rec['wire']['input']==10000 and rec['wire']['output']==1200 and rec['wire']['read']==rec['wire']['write']==0,('disabled changed raw',path))
        if saved['kind']=='usage_multiplier':
         for bucket,param in [('input','input'),('read','cache_read'),('write','cache_creation'),('output','output')]:
          raw=case['raw'][bucket];rule=saved['usage_multiplier'][param];wanted=raw if not saved['usage']['enabled'] or saved['protocol']!=protocol or raw<=rule['threshold_tokens'] else min(4000000,math.floor(raw*rule['multiplier']+.5))
          if saved['usage']['enabled'] and saved['protocol']==protocol:
           if bucket=='output':wanted=min(wanted,20000,64000 if protocol=='anthropic' else 65535,saved['limits']['max_output_tokens'] or 4000000)
           if bucket=='input':wanted=min(wanted,200000 if protocol=='anthropic' else 1048576,saved['limits']['max_input_tokens'] or 4000000)
          g.require(rec['wire'][bucket]==wanted,('direct multiplier independent expected',path,bucket,rec['wire'],wanted))
        if saved['protocol']!=protocol:g.require(rec['wire']['input']==10000 and rec['wire']['output']==1200 and rec['wire']['read']==rec['wire']['write']==0,('protocol mismatch projected',path))
       self.verify(case,records)
       trial.update(passed=True,request_ids=[x['request_id'] for x in records],usage=[x['wire'] for x in records],costs=[x['db']['total_cost'] for x in records])
      except Exception as error:
       trial.update(passed=False,error=repr(error));entry['passed']=False;self.errors.append({'protocol':protocol,'strategy':kind,'path':path,'value':value,'error':repr(error)})
       print('FAIL',protocol,kind,path,value,repr(error),flush=True)
     if not self.args.skip_invalid:
      for label,value in limits(path,initial):
       try:
        cfg=field_config(default,path,value)
        status,text,_=r.request('PUT','/api/v1/admin/cache-strategies/'+str(case['strategy']['id']),{'name':case['name'],'config':cfg,'expected_revision':case['strategy']['revision']},token=r.admin)
        accepted=status==200
        # These are intentional legacy normalizations; others must reject before persisting.
        normalized_ok=path.endswith('.normal_max_multiplier') and value<0
        if accepted:
         item=json.loads(text)['data'];case['strategy']=item;case['config']=item['config']
        g.require((400<=status<500) or accepted and normalized_ok,('invalid accepted or server error',path,label,status,text[:120]))
        entry.setdefault('invalid',[]).append({'kind':label,'value':value,'status':status,'passed':True})
       except Exception as error:
        entry['passed']=False;entry.setdefault('invalid',[]).append({'kind':label,'value':value,'passed':False,'error':repr(error)});self.errors.append({'protocol':protocol,'strategy':kind,'path':path,'invalid':label,'error':repr(error)});print('FAIL invalid',protocol,kind,path,label,repr(error),flush=True)
     if index%10==0:self.save();print('PROGRESS',protocol,kind,index+1,'/',len(leaves),'fields','failures',len(self.errors),'requests',len(r.results),flush=True)
    self.save();print('DONE',protocol,kind,len(leaves),'fields',flush=True)


def main():
 p=argparse.ArgumentParser();p.add_argument('--base',default='http://127.0.0.1:48788');p.add_argument('--mock-port',type=int,default=48791);p.add_argument('--report',default='/tmp/cache-parameter-e2e.json');p.add_argument('--filter',default='');p.add_argument('--paths',default='');p.add_argument('--skip-invalid',action='store_true');p.add_argument('--upper-only',action='store_true');args=p.parse_args()
 audit=Audit(args);r=audit.runner
 try:r.bootstrap();audit.matrix()
 except Exception as error:audit.errors.append({'fatal':repr(error)});print('FATAL',repr(error),flush=True)
 finally:
  audit.save();cleanup=r.cleanup();r.server.shutdown()
  d=json.loads(Path(args.report).read_text());d['cleanup_errors']=cleanup;d['complete']=not args.paths and len(d['coverage'])==188*len(args.filter.split(',')) if args.filter else not args.paths and len(d['coverage'])==188*14
  d['records']=r.results;Path(args.report).write_text(json.dumps(d,ensure_ascii=False,indent=2));print(json.dumps({k:v for k,v in d.items() if k not in ['coverage','records','errors']},ensure_ascii=False),flush=True)
 if audit.errors or cleanup:raise SystemExit(1)

if __name__=='__main__':main()
