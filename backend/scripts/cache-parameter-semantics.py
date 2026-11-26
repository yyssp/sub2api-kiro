#!/usr/bin/env python3
"""Behavior oracles for cache parameters, using the running gateway and database."""
import argparse,copy,importlib.util,json,math,time
from pathlib import Path
spec=importlib.util.spec_from_file_location('parameter_audit',Path(__file__).with_name('cache-parameter-e2e.py'));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
g=m.g
STATEFUL=g.KINDS[1:-1]

class Semantics(m.Audit):
 def base(self,protocol,kind):
  c=copy.deepcopy(m.DEFAULTS[protocol+'/'+kind])
  g.deep(c,{'coverage_ratio':1,'ratio_mode':'independent','usage_ratio':1,'read_ratio':1,'creation_ratio':1,'token_scale':1,'reported_input_min_tokens':0,'reported_input_max_tokens':0,'uncached_input_min_tokens':0,'uncached_input_max_tokens':0,'max_simulated_input_tokens':0,'cap_jitter_min_tokens':0,'cap_jitter_max_tokens':0,'min_cacheable_tokens':0,'cold_request_guard':False,'max_new_creation_tokens_per_request':0,'max_coverage_tokens':0,'state_ttl_seconds':0,'creation_control':{'enabled':False},'hit_shaping':{'enabled':False,'target_hit_ratio':0,'ratio_jitter':0,'min_cache_read_tokens':0,'min_cache_creation_tokens':0,'min_cost_ratio':0,'min_creation_ratio':0,'max_read_multiplier':0},'stable_segment':{'min_total_input_tokens':0,'min_segment_tokens':0,'reported_input_min_tokens':0,'reported_input_max_tokens':0},'usage':{'output_uplift_enabled':False,'final_output_guard_enabled':True,'final_cache_read_max_tokens':0,'final_cache_read_jitter_min_tokens':0,'final_cache_read_jitter_max_tokens':0,'final_cache_creation_max_tokens':0,'final_cache_creation_jitter_min_tokens':0,'final_cache_creation_jitter_max_tokens':0,'final_output_max_tokens':0,'final_output_jitter_min_tokens':0,'final_output_jitter_max_tokens':0}})
  return c
 def probe(self,protocol,kind,label,paths,patch=None,check=None,raw=None,body_fn=None,turns=2,delay=0):
  if self.args.filter and protocol+'/'+kind not in self.args.filter.split(','):return
  if self.args.probes and not any(label.startswith(p) for p in self.args.probes.split(',')):return
  e={'protocol':protocol,'strategy':kind,'probe':label,'paths':paths,'passed':False};self.coverage.append(e)
  try:
   c=self.base(protocol,kind);g.deep(c,patch or {})
   if not hasattr(self,'cases'):self.cases={}
   key=(protocol,kind)
   if key not in self.cases:self.cases[key]=self.runner.setup(protocol,kind,'semantic')
   case=self.cases[key];self.set_config(case,c)
   with g.LOCK:g.CASES[case['id']]['raw']=raw or {'input':8000,'read':4000,'write':3000 if protocol=='anthropic' else 0,'output':1200}
   case['raw']=copy.deepcopy(g.CASES[case['id']]['raw']);session=case['id']+'-'+str(len(self.coverage));records=[]
   for turn in range(turns):
    if turn>0 and delay:time.sleep(delay)
    stream=turn%2==1
    body=body_fn(self.runner,case,stream,session,turn) if body_fn else None
    rec=self.runner.forward(case,stream,session,turn,body);records.append(rec)
    if check:check(case,rec,turn)
   self.verify(case,records)
   if protocol=='anthropic' and label.startswith('cost-floor-') and kind!='prefix_tool_plus':
    rawcost=case['raw']['input']*3e-6+case['raw']['read']*.3e-6+case['raw']['write']*3.75e-6+case['raw']['output']*15e-6
    g.require(all(x['db']['total_cost']+1e-6>=rawcost*1.2 for x in records),('cost floor amount',[x['db']['total_cost'] for x in records],rawcost*1.2))
   if protocol=='anthropic' and label.startswith('plus-cost-'):
    for x in records:
     if 'non_stream' in label and x['stream']:continue
     rawcost=case['raw']['input']*3e-6+case['raw']['read']*.3e-6+case['raw']['write']*4.65e-6+case['raw']['output']*15e-6
     g.require(x['db']['total_cost']+1e-6>=rawcost*2,('plus floor amount',x['db']['total_cost'],rawcost*2))
   e.update(passed=True,usage=[x['wire'] for x in records],costs=[x['db']['total_cost'] for x in records],request_ids=[x['request_id'] for x in records])
   print('PASS',protocol,kind,label,flush=True)
  except Exception as error:
   e['error']=repr(error);self.errors.append(e.copy());print('FAIL',protocol,kind,label,repr(error),flush=True)
  self.save()
 def run(self):
  r=self.runner
  zero={'input':10000,'read':0,'write':0,'output':1200}
  def bounded(bucket,lo,hi):return lambda c,x,t:g.require(lo<=x['wire'][bucket]<=hi,(bucket,lo,hi,x['wire']))
  def positive(c,x,t):g.require((x['wire']['read']>0)==(t>0),('warm/cold',x['wire']))
  def no_cache(c,x,t):g.require(x['wire']['read']==x['wire']['write']==0,('unexpected cache',x['wire']))
  def no_system(rr,c,st,se,t):
   b=rr.body(c,st,se,t);b.pop('system',None);b.pop('systemInstruction',None);return b
  def only_source(source):
   def body(rr,c,st,se,t):
    b=rr.body(c,st,se,0);text='stable source content '*1500
    if c['protocol']=='anthropic':
     b.pop('system',None);b['messages']=[{'role':'user','content':'current'}]
     if source=='system':b['system']=[{'type':'text','text':text}]
     if source=='tools':b['tools']=[{'name':'lookup','description':text,'input_schema':{'type':'object','properties':{}}}]
     if source=='history':b['messages']=[{'role':'user','content':text},{'role':'assistant','content':'done'},{'role':'user','content':'current'}]
     if source=='tool_results':b['messages']=[{'role':'user','content':[{'type':'tool_result','tool_use_id':'tool_a','content':text}]},{'role':'user','content':'current'}]
     if source=='current_user_stable_prefix':b['messages']=[{'role':'user','content':[{'type':'text','text':text}]}]
    else:
     b.pop('systemInstruction',None);b['contents']=[{'role':'user','parts':[{'text':'current'}]}]
     if source=='system':b['systemInstruction']={'parts':[{'text':text}]}
     if source=='tools':b['tools']=[{'functionDeclarations':[{'name':'lookup','description':text,'parameters':{'type':'OBJECT','properties':{}}}]}]
     if source=='history':b['contents']=[{'role':'user','parts':[{'text':text}]},{'role':'model','parts':[{'text':'done'}]},{'role':'user','parts':[{'text':'current'}]}]
     if source=='tool_results':b['contents']=[{'role':'user','parts':[{'functionResponse':{'name':'lookup','response':{'text':text}}}]},{'role':'user','parts':[{'text':'current'}]}]
     if source=='current_user_stable_prefix':b['contents']=[{'role':'user','parts':[{'text':text}]}]
    return b
   return body
  def dynamic(rr,c,st,se,t):
   b=only_source('tool_results')(rr,c,st,se,t)
   if c['protocol']=='anthropic':b['messages'][0]['content'][0]={'type':'tool_result','tool_use_id':'tool_a','volatile':True,'content':'dynamic '*2000}
   else:b['contents'][0]['parts'][0]['functionResponse']['name']='web_search'
   return b
  def incremental(rr,c,st,se,t):
   b=rr.body(c,st,se,0)
   if c['protocol']=='anthropic':
    b['system'][0]['cache_control']={'type':'ephemeral','ttl':'5m'}
    b['messages']=[{'role':'user','content':'seed stable'}]
    for n in range(t+1):b['messages'] += [{'role':'assistant','content':'history '+str(n)+' payload '*500},{'role':'user','content':'question '+str(n)}]
   else:
    b['contents']=[{'role':'user','parts':[{'text':'seed stable'}]}]
    for n in range(t+1):b['contents'] += [{'role':'model','parts':[{'text':'history '+str(n)+' payload '*500}]},{'role':'user','parts':[{'text':'question '+str(n)}]}]
   return b
  for protocol in ['anthropic','gemini']:
   for kind in STATEFUL:
    self.probe(protocol,kind,'output-guard-on',['usage.final_output_guard_enabled','usage.final_output_max_tokens'],{'usage':{'final_output_max_tokens':500}},bounded('output',500,500))
    self.probe(protocol,kind,'output-guard-off',['usage.final_output_guard_enabled'],{'usage':{'final_output_guard_enabled':False,'final_output_max_tokens':500}},bounded('output',1200,1200))
    for on in [False,True]:
     self.probe(protocol,kind,'uplift-'+str(on),['usage.output_uplift_enabled','usage.output_uplift_min_tokens','usage.output_uplift_percent'],{'usage':{'output_uplift_enabled':on,'output_uplift_min_tokens':1000,'output_uplift_percent':50}},bounded('output',1800 if on and kind!='prefix_tool_plus' else 1200,1800 if on and kind!='prefix_tool_plus' else 1200))
    for bucket,field in [('input','input'),('output','output'),('read','cache_read')]+([('write','cache_creation')] if protocol=='anthropic' else []):
     for mode in ['sample_max','sample_target']:
      patch={'usage':{field:{'mode':mode,'max_tokens':400,'target_tokens':400,'normal_max_multiplier':1.1}}}
      self.probe(protocol,kind,f'sample-{field}-{mode}',['usage.'+field+'.mode','usage.'+field+'.max_tokens','usage.'+field+'.target_tokens','usage.'+field+'.normal_max_multiplier'],patch,bounded(bucket,0,440 if mode=='sample_target' else 400))
    for field,bucket in [('cache_read','read'),('cache_creation','write')]+[('output','output')]:
     if protocol=='gemini' and bucket=='write':continue
     for jitter in [(0,0),(10,20),(700,800)]:
      patch={'usage':{'final_'+field+'_max_tokens':500,'final_'+field+'_jitter_min_tokens':jitter[0],'final_'+field+'_jitter_max_tokens':jitter[1]}}
      # Input/write transformations can be lower than a cap, so assert the ceiling and positivity only when present.
      self.probe(protocol,kind,f'final-{field}-{jitter[0]}',['usage.final_'+field+'_max_tokens','usage.final_'+field+'_jitter_min_tokens','usage.final_'+field+'_jitter_max_tokens'],patch,bounded(bucket,0,500))
    for source in ['system','tools','history','tool_results','current_user_stable_prefix']:
     for on in [False,True]:
      flags={f'cache_{x}':False for x in ['system','tools','history','tool_results','current_user_stable_prefix']}
      # Keep one unrelated source enabled to meet schema validation without adding that source to the fixture.
      flags['cache_system' if source!='system' else 'cache_tools']=True
      if source=='tool_results':flags['cache_history']=True
      flags['cache_'+source]=on
      if source=='current_user_stable_prefix':flags['current_user_stable_prefix_max_tokens']=100000
      self.probe(protocol,kind,f'source-{source}-{on}',list(flags),flags,positive if on else no_cache,raw=zero,body_fn=only_source(source))
    for threshold in [0,1000000]:
     self.probe(protocol,kind,'minimum-'+str(threshold),['min_cacheable_tokens'],{'min_cacheable_tokens':threshold},positive if threshold==0 else no_cache,raw=zero)
     model='claude-sonnet-4-5' if protocol=='anthropic' else 'gemini-2.5-pro'
     self.probe(protocol,kind,'model-minimum-'+str(threshold),['model_min_cacheable_overrides'],{'min_cacheable_tokens':1000000 if threshold==0 else 0,'model_min_cacheable_overrides':{model:threshold}},positive if threshold==0 else no_cache,raw=zero)
    for mode in ['exclude','allow']:
     self.probe(protocol,kind,'dynamic-'+mode,['dynamic_content_mode'],{'dynamic_content_mode':mode},no_cache if mode=='exclude' else positive,raw=zero,body_fn=dynamic)
    for allow in [False,True]:
     def sessionless(rr,c,st,se,t):
      b=rr.body(c,st,se,t);b.pop('metadata',None);return b
     self.probe(protocol,kind,'derived-'+str(allow),['allow_derived_session'],{'allow_derived_session':allow},positive if allow else no_cache,raw=zero,body_fn=sessionless)
    for path in ['state_ttl_seconds','default_ttl_seconds','expire_after_idle_seconds']:
     def expiry_body(rr,c,st,se,t):
      b=rr.body(c,st,se,t)
      if path=='default_ttl_seconds' and c['protocol']=='anthropic':
       for block in b.get('system',[]):block.pop('cache_control',None)
      return b
     self.probe(protocol,kind,'expiry-'+path,[path],{path:1,'state_ttl_seconds':1 if path=='state_ttl_seconds' else 0,'breakpoint_mode':'auto'},no_cache if protocol=='gemini' else lambda c,x,t:g.require(x['wire']['read']==0,('expired cache',x['wire'])),raw=zero,delay=1.1,body_fn=expiry_body)
    for path in ['estimated_bytes_limit','max_entries_global','max_entries_per_scope']:
     patch={path:1}
     if path=='estimated_bytes_limit':
      self.probe(protocol,kind,'resource-'+path,[path],patch,lambda c,x,t:g.require(x['wire']['read']==0,('eviction',x['wire'])),raw=zero)
     else:
      def a_b_a(rr,c,st,se,t):
       b=only_source('system')(rr,c,st,se,t);text=('variant A ' if t!=1 else 'variant B ')*3000
       if c['protocol']=='anthropic':b['system'][0]['text']=text
       else:b['systemInstruction']['parts'][0]['text']=text;b['session_id']=se
       return b
      self.probe(protocol,kind,'resource-'+path,[path],patch,lambda c,x,t:g.require(x['wire']['read']==0,('LRU eviction',x['wire'])),raw=zero,body_fn=a_b_a,turns=3)
    self.probe(protocol,kind,'incremental-off',['incremental_create_enabled'],{'incremental_create_enabled':False},lambda c,x,t:g.require(t==0 or x['wire']['write']==0,('incremental disabled',x['wire'])),raw=zero,body_fn=incremental,turns=3)
    for cap in [0,500]:
     self.probe(protocol,kind,'coverage-cap-'+str(cap),['max_coverage_tokens'],{'max_coverage_tokens':cap},bounded('read',0,cap if cap else 200000),raw=zero)
    for ratio in [0,.5,1]:
     self.probe(protocol,kind,'read-ratio-'+str(ratio),['ratio_mode','read_ratio','usage_ratio'],{'ratio_mode':'independent','read_ratio':ratio},bounded('read',0,0) if ratio==0 else positive,raw=zero)
    for on in [False,True]:
     self.probe(protocol,kind,'preserve-'+str(on),['preserve_upstream_cache_usage','usage.preserve_upstream_cache_usage'],{'preserve_upstream_cache_usage':on},lambda c,x,t,keep=on:g.require(not keep or kind=='prefix_tool_plus' and protocol=='anthropic' or (x['wire']['input']==8000 and x['wire']['read']==4000 and x['wire']['write']==(3000 if protocol=='anthropic' else 0)),('preserve',x['wire'])))
    for path in ['limits.max_input_tokens','limits.max_output_tokens']:
     patch={};m.put(patch,path,100)
     self.probe(protocol,kind,'hard-'+path,[path],patch,lambda c,x,t,p=path:g.require(sum(x['wire'][k] for k in ['input','read','write'])<=100 if p.endswith('input_tokens') else x['wire']['output']<=100,('hard limit',p,x['wire'])))
    for total in [1000,20000]:
     self.probe(protocol,kind,'reported-fixed-'+str(total),['reported_input_min_tokens','reported_input_max_tokens','token_scale','max_simulated_input_tokens'],{'reported_input_min_tokens':total,'reported_input_max_tokens':total},lambda c,x,t,n=total:g.require(kind=='stable_segment_cache' and protocol=='anthropic' or sum(x['wire'][k] for k in ['input','read','write'])==n,('reported fixed total',x['wire'],n)),raw=zero)
    for factor in [1,3]:
     self.probe(protocol,kind,'scale-'+str(factor),['token_scale','scale_min_input_tokens','max_simulated_input_tokens'],{'token_scale':factor,'scale_min_input_tokens':0,'max_simulated_input_tokens':5000},lambda c,x,t:g.require(sum(x['wire'][k] for k in ['input','read','write'])<=5000,('simulated cap',x['wire'])),raw=zero)
    for mode in ['client_only','auto','hybrid']:
     self.probe(protocol,kind,'breakpoint-'+mode,['breakpoint_mode'],{'breakpoint_mode':mode},no_cache if mode=='client_only' and protocol=='anthropic' and kind!='stable_segment_cache' else positive,raw=zero,body_fn=only_source('system'))
    for stream_flag in ['strict_cache_eligibility_stream','strict_cache_eligibility_non_stream']:
     def short(rr,c,st,se,t):
      b=rr.body(c,st,se,t);b.pop('system',None);b.pop('systemInstruction',None)
      if c['protocol']=='anthropic':b['messages']=[{'role':'user','content':'hi'}]
      else:b['contents']=[{'role':'user','parts':[{'text':'hi'}]}]
      return b
     self.probe(protocol,kind,'strict-'+stream_flag,['usage.'+stream_flag],{'usage':{stream_flag:True,'output_uplift_enabled':True,'output_uplift_percent':50,'output_uplift_min_tokens':1000}},lambda c,x,t,flag=stream_flag:g.require((x['stream'] if flag.endswith('_stream') and not flag.endswith('non_stream') else not x['stream'])==False or x['wire']['output']==1200,('strict eligibility',x['wire'])),raw=zero,body_fn=short)
    self.probe(protocol,kind,'skip-non-stream',['usage.skip_non_stream_usage_projection'],{'usage':{'skip_non_stream_usage_projection':True,'output_uplift_enabled':True,'output_uplift_percent':50,'output_uplift_min_tokens':1000}},lambda c,x,t:g.require(x['stream'] or x['wire']['output']==1200,('skip json',x['wire'])),raw=zero)
    for enabled in [False,True]:
     self.probe(protocol,kind,'move-input-delta-'+str(enabled),['usage.input.move_delta_to_cache_read'],{'usage':{'input':{'mode':'sample_max','max_tokens':500,'move_delta_to_cache_read':enabled}}},lambda c,x,t,on=enabled:g.require(not on or kind=='prefix_tool_plus' and protocol=='anthropic' or x['wire']['read']>4000,('move delta read',x['wire'])))
    for target in [.5,.9]:
     self.probe(protocol,kind,'hit-target-'+str(target),['hit_shaping.enabled','hit_shaping.target_hit_ratio','hit_shaping.ratio_jitter'],{'hit_shaping':{'enabled':True,'target_hit_ratio':target,'ratio_jitter':0}},lambda c,x,t,goal=target:g.require(t==0 or kind=='prefix_tool_plus' or sum(x['wire'][k] for k in ['input','read','write'])==0 or x['wire']['read']/sum(x['wire'][k] for k in ['input','read','write'])>=goal-.02,('hit target',goal,x['wire'])),raw=zero)
    for flag in ['read_aware_cost_floor','cost_floor_top_up_input']:
     self.probe(protocol,kind,'cost-floor-'+flag,['hit_shaping.'+flag,'hit_shaping.min_cost_ratio','hit_shaping.min_cache_read_tokens','hit_shaping.min_cache_creation_tokens','hit_shaping.max_read_multiplier','hit_shaping.min_creation_ratio'],{'hit_shaping':{'enabled':True,flag:True,'target_hit_ratio':.8,'ratio_jitter':.02,'min_cost_ratio':1.2,'min_cache_read_tokens':100,'min_cache_creation_tokens':100,'max_read_multiplier':2,'min_creation_ratio':.01}},lambda c,x,t:g.require(all(v>=0 for v in x['wire'].values()),('cost floor nonnegative',x['wire'])),raw=zero)
    if protocol=='anthropic':
     for ttl in ['5m','1h']:
      self.probe(protocol,kind,'force-ttl-'+ttl,['forced_ttl_tier'],{'forced_ttl_tier':ttl},lambda c,x,t,tier=ttl:g.require(x['wire'][tier]==x['wire']['write'] and x['wire']['1h' if tier=='5m' else '5m']==0,('forced ttl',x['wire'])))
     for trust in [False,True]:
      self.probe(protocol,kind,'trust-ttl-'+str(trust),['trust_upstream_ttl_tier'],{'trust_upstream_ttl_tier':trust},lambda c,x,t:g.require(x['wire']['5m']+x['wire']['1h']==x['wire']['write'],('TTL conservation',x['wire'])))
     for control,patch in [('min_creation_delta_tokens',{'min_creation_delta_tokens':1000000,'max_creation_tokens_per_event':0}),('max_creation_tokens_per_event',{'max_creation_tokens_per_event':500}),('min_creation_interval_seconds',{'min_creation_interval_seconds':60}),('min_successful_requests_between',{'min_successful_requests_between':2}),('max_creation_tokens_per_window',{'max_creation_tokens_per_window':500,'creation_budget_window_seconds':60})]:
      expected=lambda c,x,t,k=control:g.require(x['wire']['write']==0 if k in ['min_creation_delta_tokens','min_creation_interval_seconds','min_successful_requests_between'] and t>0 else x['wire']['write']<=500 if k in ['max_creation_tokens_per_event','max_creation_tokens_per_window'] else True,('creation control',k,x['wire']))
      self.probe(protocol,kind,'creation-'+control,['creation_control.enabled','creation_control.'+control],{'creation_control':{'enabled':True,**patch}},expected,raw=zero,body_fn=incremental,turns=3)
    for ratio in [0,.5,1]:
     self.probe(protocol,kind,'creation-ratio-'+str(ratio),['creation_ratio','ratio_mode'],{'ratio_mode':'independent','creation_ratio':ratio},bounded('write',0,0) if ratio==0 else None,raw=zero)
     self.probe(protocol,kind,'coverage-ratio-'+str(ratio),['coverage_ratio'],{'coverage_ratio':ratio},no_cache if ratio==0 and kind!='stable_segment_cache' else None,raw=zero)
    for cold in [False,True]:
     self.probe(protocol,kind,'cold-guard-'+str(cold),['cold_request_guard'],{'cold_request_guard':cold,'token_scale':3,'scale_min_input_tokens':0},lambda c,x,t,on=cold:g.require(not on or t>0 or x['wire']['read']==0 and x['wire']['input']<=10000,('cold guard',x['wire'])),raw=zero)
    for floor in [(0,0),(700,900)]:
     self.probe(protocol,kind,'uncached-band-'+str(floor[0]),['uncached_input_min_tokens','uncached_input_max_tokens'],{'uncached_input_min_tokens':floor[0],'uncached_input_max_tokens':floor[1]},lambda c,x,t,band=floor:g.require(band[0]==0 or x['wire']['input']>=band[0],('uncached floor',x['wire'])),raw=zero)
    for cap in [0,100]:
     self.probe(protocol,kind,'write-set-cap-'+str(cap),['max_new_creation_tokens_per_request'],{'max_new_creation_tokens_per_request':cap},lambda c,x,t,n=cap:g.require(protocol=='gemini' or n==0 or x['wire']['write']<=n or kind=='stable_segment_cache' and x['wire']['read']==0 and x['wire']['write']<=n,('write set cap',x['wire'])),raw=zero)
    for jitter in [(0,0),(10,20)]:
     self.probe(protocol,kind,'input-cap-jitter-'+str(jitter[1]),['cap_jitter_min_tokens','cap_jitter_max_tokens'],{'reported_input_max_tokens':5000,'cap_jitter_min_tokens':jitter[0],'cap_jitter_max_tokens':jitter[1]},lambda c,x,t:g.require(sum(x['wire'][k] for k in ['input','read','write'])<=5000,('input cap jitter',x['wire'])),raw=zero)
    for enabled in [False,True]:
     self.probe(protocol,kind,'usage-toggle-'+str(enabled),['usage.enabled'],{'usage':{'enabled':enabled,'output_uplift_enabled':True,'output_uplift_min_tokens':1000,'output_uplift_percent':50}},lambda c,x,t,on=enabled:g.require(on or x['wire']['output']==1200 and x['wire']['input']==10000 and x['wire']['read']==x['wire']['write']==0,('usage disabled',x['wire'])),raw=zero)
    if protocol=='anthropic':
     def hour_marker(rr,c,st,se,t):
      b=rr.body(c,st,se,t);b['system'][0]['cache_control']={'type':'ephemeral','ttl':'1h'};return b
     self.probe(protocol,kind,'hour-ttl-expiry',['hour_ttl_seconds'],{'default_ttl_seconds':1,'hour_ttl_seconds':1,'state_ttl_seconds':0},lambda c,x,t:g.require(x['wire']['read']==0,('hour TTL expiry',x['wire'])),raw=zero,body_fn=hour_marker,delay=1.1)
     self.probe(protocol,kind,'creation-window-reset',['creation_control.creation_budget_window_seconds','creation_control.max_creation_tokens_per_window'],{'creation_control':{'enabled':True,'creation_budget_window_seconds':1,'max_creation_tokens_per_window':500}},lambda c,x,t:g.require(0<x['wire']['write']<=500,('budget window release',x['wire'])),raw=zero,body_fn=incremental,delay=1.1)
    if kind=='stable_segment_cache':
     for path in ['min_total_input_tokens','min_segment_tokens']:
      self.probe(protocol,kind,'stable-'+path,['stable_segment.'+path],{'stable_segment':{path:1000000}},no_cache,raw=zero)
     for source,flag in [('system','auto_cache_system'),('tools','auto_cache_tools'),('history','auto_cache_history_message_ends')]:
      self.probe(protocol,kind,'stable-auto-off-'+source,['stable_segment.'+flag],{'stable_segment':{flag:False,'honor_explicit_cache_control':False}},no_cache,raw=zero,body_fn=only_source(source))
     self.probe(protocol,kind,'stable-input-band',['stable_segment.reported_input_min_tokens','stable_segment.reported_input_max_tokens'],{'stable_segment':{'reported_input_min_tokens':64,'reported_input_max_tokens':96}},lambda c,x,t:g.require(t==0 and protocol=='gemini' or 64<=x['wire']['input']<=96,('stable band',x['wire'])),raw=zero)
     for limit in [0,1]:
      self.probe(protocol,kind,'stable-history-limit-'+str(limit),['stable_segment.history_message_limit'],{'stable_segment':{'history_message_limit':limit}},(lambda c,x,t:g.require(x['wire']['read']+x['wire']['write']<100,('history limit excludes old large message',x['wire']))) if limit==1 else positive,raw=zero,body_fn=only_source('history'))
     for honor in [False,True]:
      def explicit_system(rr,c,st,se,t):
       b=only_source('system')(rr,c,st,se,t)
       if c['protocol']=='anthropic':b['system'][0]['cache_control']={'type':'ephemeral'}
       return b
      self.probe(protocol,kind,'stable-explicit-'+str(honor),['stable_segment.honor_explicit_cache_control'],{'stable_segment':{'auto_cache_system':False,'auto_cache_tools':False,'auto_cache_history_message_ends':False,'honor_explicit_cache_control':honor}},positive if honor and protocol=='anthropic' else no_cache,raw=zero,body_fn=explicit_system)
    if kind=='prefix_tool_plus':
     self.plus(protocol,kind,bounded)
 def plus(self,protocol,kind,bounded):
  fields=['input','output','cache_read']+(['cache_creation'] if protocol=='anthropic' else [])
  for profile in ['shaping','non_stream.profile','small_request.profile']:
   for field in fields:
    bucket={'cache_read':'read','cache_creation':'write'}.get(field,field)
    for key,value in [('multiplier',0),('multiplier',2),('jitter_ratio',.2),('min_tokens',100),('max_tokens',100),('max_jitter_tokens',20)]:
     patch={};path='tool_plus.'+profile+'.'+field+'.'+key;m.put(patch,path,value)
     if profile=='non_stream.profile':m.put(patch,'tool_plus.non_stream.mode','custom')
     if profile=='small_request.profile':m.put(patch,'tool_plus.small_request.enabled',True);m.put(patch,'tool_plus.small_request.max_input_tokens',4000000);m.put(patch,'tool_plus.small_request.mode','custom')
     if key=='max_jitter_tokens':m.put(patch,'tool_plus.'+profile+'.'+field+'.max_tokens',500)
     def check(c,x,t,k=key,b=bucket,v=value,pr=profile):
      if pr=='non_stream.profile' and x['stream']:return
      if k=='multiplier' and v==0:g.require(x['wire'][b]==0,('plus zero multiplier',pr,b,x['wire']))
      if k=='max_tokens':g.require(x['wire'][b]<=v,('plus max',pr,b,x['wire']))
      if k=='max_jitter_tokens':g.require(x['wire'][b]<=500,('plus jitter max',pr,b,x['wire']))
     self.probe(protocol,kind,'plus-'+profile+'-'+field+'-'+key+'-'+str(value),[path],patch,check)

  for profile in ['shaping','non_stream.profile','small_request.profile']:
   for overflow in [False,True]:
    patch={};root='tool_plus.'+profile
    m.put(patch,root+'.cap_overflow_to_input',overflow);m.put(patch,root+'.cache_read.max_tokens',100);m.put(patch,root+'.cache_creation.max_tokens',100)
    if profile=='non_stream.profile':m.put(patch,'tool_plus.non_stream.mode','custom')
    if profile=='small_request.profile':m.put(patch,'tool_plus.small_request.enabled',True);m.put(patch,'tool_plus.small_request.max_input_tokens',4000000);m.put(patch,'tool_plus.small_request.mode','custom')
    self.probe(protocol,kind,'plus-overflow-'+profile+'-'+str(overflow),[root+'.cap_overflow_to_input'],patch,lambda c,x,t:g.require(all(v>=0 for v in x['wire'].values()),('overflow',x['wire'])))
   patch={};m.put(patch,'tool_plus.'+profile+'.min_cost_ratio',2)
   if profile=='non_stream.profile':m.put(patch,'tool_plus.non_stream.mode','custom')
   if profile=='small_request.profile':m.put(patch,'tool_plus.small_request.enabled',True);m.put(patch,'tool_plus.small_request.max_input_tokens',4000000);m.put(patch,'tool_plus.small_request.mode','custom')
   self.probe(protocol,kind,'plus-cost-'+profile,['tool_plus.'+profile+'.min_cost_ratio'],patch)
  for target in ['input','cache_read','drop']:
   self.probe(protocol,kind,'plus-suppressed-'+target,['tool_plus.suppressed_creation_target'],{'creation_control':{'enabled':True,'max_creation_tokens_per_event':100},'tool_plus':{'suppressed_creation_target':target}},lambda c,x,t:g.require(x['wire']['write']<=100 if protocol=='anthropic' else x['wire']['write']==0,('suppressed write target',target,x['wire'])),raw={'input':10000,'read':0,'write':0,'output':1200})
  for mode in ['inherit','custom','raw']:
   self.probe(protocol,kind,'plus-nonstream-'+mode,['tool_plus.non_stream.mode'],{'tool_plus':{'non_stream':{'mode':mode,'profile':{'output':{'multiplier':2}}}}},lambda c,x,t,md=mode:g.require(x['wire']['output']==(2400 if md=='custom' and not x['stream'] else 1200),('nonstream override',md,x['wire'])))
   self.probe(protocol,kind,'plus-small-'+mode,['tool_plus.small_request.enabled','tool_plus.small_request.mode','tool_plus.small_request.max_input_tokens'],{'tool_plus':{'small_request':{'enabled':True,'mode':mode,'max_input_tokens':4000000,'profile':{'output':{'multiplier':2}}}}},lambda c,x,t,md=mode:g.require(x['wire']['output']==(2400 if md=='custom' else 1200),('small override',md,x['wire'])))
  self.probe(protocol,kind,'plus-small-disable',['tool_plus.small_request.disable_cache'],{'tool_plus':{'small_request':{'enabled':True,'disable_cache':True,'max_input_tokens':4000000}}},lambda c,x,t:g.require(x['wire']['read']==x['wire']['write']==0,('small disable',x['wire'])),raw={'input':10000,'read':0,'write':0,'output':1200})


def main():
 p=argparse.ArgumentParser();p.add_argument('--base',default='http://127.0.0.1:48788');p.add_argument('--mock-port',type=int,default=48791);p.add_argument('--report',default='/tmp/cache-parameter-semantics.json');p.add_argument('--filter',default='');p.add_argument('--probes',default='');args=p.parse_args()
 a=Semantics(args);r=a.runner
 try:r.bootstrap();a.run()
 except Exception as error:a.errors.append({'fatal':repr(error)});print('FATAL',repr(error),flush=True)
 finally:
  a.save();cleanup=r.cleanup();r.server.shutdown();d=json.loads(Path(args.report).read_text());d['cleanup_errors']=cleanup;d['records']=r.results;Path(args.report).write_text(json.dumps(d,ensure_ascii=False,indent=2));print('FINISH',len(a.coverage),'probes',len(a.errors),'errors',len(r.results),'requests',flush=True)
 if a.errors or cleanup:raise SystemExit(1)
if __name__=='__main__':main()
