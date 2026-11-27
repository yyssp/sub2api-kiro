#!/usr/bin/env python3
"""Exercise the running gateway against local mock accounts, including persisted billing.
No paid upstream calls. Credentials stay in memory. Only run-prefixed resources are removed.
"""
import argparse,copy,json,math,os,secrets,threading,time,urllib.request,urllib.error,urllib.parse
from http.server import BaseHTTPRequestHandler,ThreadingHTTPServer
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor

KINDS=['disabled','prefix','tool_aware','stable_segment_cache','prefix_tool_plus','bounded_high_cache','usage_multiplier']
CASES={};CALLS=[];LOCK=threading.Lock()
class MockServer(ThreadingHTTPServer):
 request_queue_size=128

class Mock(BaseHTTPRequestHandler):
 protocol_version='HTTP/1.1'
 def log_message(self,*a):pass
 def do_DELETE(self):
  self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Connection','close');self.end_headers();self.wfile.write(b'{}')
 def do_POST(self):
  body=json.loads(self.rfile.read(int(self.headers.get('Content-Length',0))))
  key=self.path.split('/')[1]
  with LOCK:
   cfg=copy.deepcopy(CASES[key]);CALLS.append({'case':key,'path':self.path,'time':time.time()})
   if cfg.get('mock')=='http_once':CASES[key]['mock']='normal'
  raw=cfg.get('raw',{'input':10000,'output':1200,'read':0,'write':0});gemini='gemini' in self.path
  mode=cfg.get('mock','normal');stream=bool(body.get('stream')) or ':streamGenerateContent' in self.path
  status=400 if mode=='http_400' else 503 if mode in ['http_error','http_once'] else 200
  self.send_response(status);self.send_header('Content-Type','text/event-stream' if stream and status==200 else 'application/json');self.send_header('x-request-id','mock-'+secrets.token_hex(12));self.send_header('Connection','close');self.end_headers()
  if 'cachedContents' in self.path:
   payload={'name':'cachedContents/mock-'+secrets.token_hex(8),'model':body.get('model','models/gemini-2.5-pro'),'usageMetadata':{'totalTokenCount':2000},'expireTime':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime(time.time()+300))}
   self.wfile.write(json.dumps(payload).encode());return
  if status!=200:self.wfile.write(b'{"error":{"type":"invalid_request_error","message":"mock failure"}}');return
  if gemini:
   usage={'promptTokenCount':raw['input']+raw['read'],'cachedContentTokenCount':raw['read'],'candidatesTokenCount':raw['output']*5//6,'thoughtsTokenCount':raw['output']-raw['output']*5//6,'totalTokenCount':raw['input']+raw['read']+raw['output']}
   parts=[{'text':cfg.get('response_text','mock response')}]
   if cfg.get('tool_output'):parts=[{'functionCall':{'name':'lookup','args':{'query':'test'}}}]
   candidate={'index':0,'content':{'role':'model','parts':parts}}
   if mode!='incomplete':candidate['finishReason']='STOP'
   if mode=='blocked':candidate['finishReason']='SAFETY'
   result={'modelVersion':'gemini-2.5-pro','candidates':[candidate],'usageMetadata':usage}
   if mode=='missing_usage':result.pop('usageMetadata',None)
   if stream:
    if mode=='multiframe':
     for n in [999,1000,1001]:
      item=copy.deepcopy(result);item['usageMetadata']={'candidatesTokenCount':n}
      if n==999:item['usageMetadata']['promptTokenCount']=raw['input']+raw['read'];item['usageMetadata']['cachedContentTokenCount']=raw['read']
      if n!=1001:item['candidates'][0].pop('finishReason',None)
      self.wfile.write(('data: '+json.dumps(item)+'\n\n').encode());self.wfile.flush()
    elif mode=='terminal_no_usage':
     initial=copy.deepcopy(result);initial['candidates'][0].pop('finishReason',None);initial['candidates'][0]['content']['parts']=[]
     self.wfile.write(('data: '+json.dumps(initial)+'\n\n').encode());self.wfile.flush()
     result.pop('usageMetadata',None)
     self.wfile.write(('data: '+json.dumps(result)+'\n\n').encode())
    else:self.wfile.write(('data: '+json.dumps(result)+'\n\n').encode())
   else:self.wfile.write(json.dumps(result).encode())
  else:
   usage={'input_tokens':raw['input'],'output_tokens':raw['output'],'cache_read_input_tokens':raw['read'],'cache_creation_input_tokens':raw['write'],'cache_creation':{'ephemeral_5m_input_tokens':raw['write']*3//5,'ephemeral_1h_input_tokens':raw['write']-raw['write']*3//5}}
   result={'id':'msg_mock_'+secrets.token_hex(6),'type':'message','role':'assistant','model':body.get('model','claude-sonnet-4-5'),'content':([{'type':'tool_use','id':'tool_mock','name':'lookup','input':{'query':'test'}}] if cfg.get('tool_output') else [{'type':'text','text':cfg.get('response_text','mock response')}]),'stop_reason':('tool_use' if cfg.get('tool_output') else 'end_turn'),'stop_sequence':None,'usage':usage}
   if mode=='missing_usage':result.pop('usage',None)
   if stream:
    start=copy.deepcopy(result);start['content']=[];start['stop_reason']=None
    if 'usage' in start:start['usage']['output_tokens']=0
    block=({'type':'tool_use','id':'tool_mock','name':'lookup','input':{}} if cfg.get('tool_output') else {'type':'text','text':''})
    delta=({'type':'input_json_delta','partial_json':json.dumps({'query':'test'})} if cfg.get('tool_output') else {'type':'text_delta','text':cfg.get('response_text','mock response')})
    events=[('message_start',{'type':'message_start','message':start}),('content_block_start',{'type':'content_block_start','index':0,'content_block':block}),('content_block_delta',{'type':'content_block_delta','index':0,'delta':delta}),('content_block_stop',{'type':'content_block_stop','index':0})]
    if mode!='incomplete':
     final={'type':'message_delta','delta':{'stop_reason':result['stop_reason']}}
     if mode not in ['missing_usage','terminal_no_usage']:final['usage']=usage
     events.extend([('message_delta',final),('message_stop',{'type':'message_stop'})])
    for event,data in events:self.wfile.write(('event: '+event+'\ndata: '+json.dumps(data)+'\n\n').encode());self.wfile.flush()
   else:self.wfile.write(json.dumps(result).encode())

class Runner:
 def __init__(self,args):
  self.args=args;self.base=args.base.rstrip('/');self.prefix='__cache_e2e_'+str(time.time_ns());self.resources=[];self.results=[];self.failures=[];self.http=[];self.checkpoints=[];self.admin=None;self.shared_key=None
  self.server=MockServer(('127.0.0.1',args.mock_port),Mock);threading.Thread(target=self.server.serve_forever,daemon=True).start()
 def request(self,method,path,data=None,token=None,key=None,timeout=30):
  headers={'Content-Type':'application/json','User-Agent':'claude-cli/2.1.0 (external, cli)','anthropic-version':'2023-06-01'}
  if key:headers['x-api-key']=key;headers['Authorization']='Bearer '+key
  elif token:headers['Authorization']='Bearer '+token
  req=urllib.request.Request(self.base+path,data=None if data is None else json.dumps(data).encode(),method=method,headers=headers)
  started=time.monotonic()
  try:
   try:
    with urllib.request.urlopen(req,timeout=timeout) as r:result=(r.status,r.read().decode(),dict(r.headers))
   except urllib.error.HTTPError as e:result=(e.code,e.read().decode(),dict(e.headers))
  except Exception as error:
   if key:self.http.append({'method':method,'path':path,'status':None,'error':type(error).__name__,'seconds':round(time.monotonic()-started,3)})
   raise
  if key:self.http.append({'method':method,'path':path,'status':result[0],'seconds':round(time.monotonic()-started,3)})
  return result
 def api(self,method,path,data=None,user=False):
  status,text,headers=self.request(method,'/api/v1'+path,data,self.user_token if user else self.admin)
  if status not in [200,201]:raise RuntimeError(f'{method} {path} HTTP {status}: {text[:500]}')
  return json.loads(text).get('data')
 def bootstrap(self):
  self.admin=self.api('POST','/auth/login',{'email':os.getenv('CACHE_E2E_ADMIN_EMAIL','dev-admin@local.test'),'password':os.getenv('CACHE_E2E_ADMIN_PASSWORD','KiroImport#2026')})['access_token']
  password=secrets.token_urlsafe(20);email=self.prefix+'@local.test'
  user=self.api('POST','/admin/users',{'email':email,'password':password,'role':'user','balance':1000,'concurrency':50})
  self.user_id=user['id'];self.resources.append(('user',user['id']))
  self.user_token=self.api('POST','/auth/login',{'email':email,'password':password})['access_token']
 def setup(self,protocol,kind,variant,patch=None,raw=None,mock='normal',wire=None):
  cid=f'{protocol}-{kind}-{variant}-{len(CASES)}';name=self.prefix+'_'+cid
  cfg=self.api('GET',f'/admin/cache-strategies/defaults?protocol={protocol}&kind={kind}')
  deep(cfg,patch or {})
  strategy=self.api('POST','/admin/cache-strategies',{'name':name,'config':cfg,'enabled':True});self.resources.append(('strategy',strategy['id']))
  platform='gemini' if protocol=='gemini' else 'anthropic'
  group=self.api('POST','/admin/groups',{'name':name,'platform':platform,'rate_multiplier':1,'subscription_type':'standard','cache_strategy_id':strategy['id']});self.resources.append(('group',group['id']))
  with LOCK:CASES[cid]={'raw':raw or {'input':10000,'output':1200,'read':0,'write':0},'mock':mock}
  account=self.api('POST','/admin/accounts',{'name':name,'platform':platform,'type':'apikey','credentials':{'api_key':'mock-not-a-real-key','explicit_cache_upstream':True,'base_url':f'http://127.0.0.1:{self.args.mock_port}/{cid}/'+platform},'extra':{},'group_ids':[group['id']],'concurrency':30,'priority':1,'status':'active','schedulable':True,'upstream_billing_probe_enabled':False,'upstream_billing_group_sync_enabled':False});self.resources.append(('account',account['id']))
  if self.shared_key is None:
   self.shared_key=self.api('POST','/keys',{'name':self.prefix,'group_id':group['id']},user=True);self.resources.append(('key',self.shared_key['id']))
  else:self.api('PUT','/keys/'+str(self.shared_key['id']),{'group_id':group['id']},user=True)
  key=self.shared_key
  return {'id':cid,'name':name,'protocol':protocol,'kind':kind,'variant':variant,'config':strategy['config'],'strategy':strategy,'group':group,'account':account,'key':key,'raw':CASES[cid]['raw'],'wire':wire or protocol}
 def body(self,case,stream,session,turn=0,maxout=20000):
  stable='stable cache prefix source code instructions '*500
  if case['protocol']=='gemini':
   return {'systemInstruction':{'parts':[{'text':stable}]},'contents':[{'role':'user','parts':[{'text':'first question '+session}]},{'role':'model','parts':[{'text':'answer stable'}]},{'role':'user','parts':[{'text':'current '+str(turn)}]}],'generationConfig':{'maxOutputTokens':maxout}}
  return {'model':'claude-sonnet-4-5','stream':stream,'max_tokens':maxout,'metadata':{'session_id':session},'system':[{'type':'text','text':stable,'cache_control':{'type':'ephemeral'}}],'messages':[{'role':'user','content':'first question '+session},{'role':'assistant','content':'answer stable'},{'role':'user','content':'current '+str(turn)}]}
 def forward(self,case,stream,session,turn=0,body=None):
  path='/v1/messages'
  if case['protocol']=='gemini':path='/v1beta/models/gemini-2.5-pro:'+('streamGenerateContent?alt=sse' if stream else 'generateContent')
  elif case['wire']=='chat':path='/v1/chat/completions'
  elif case['wire']=='responses':path='/v1/responses'
  payload=body or self.body(case,stream,session,turn)
  if case['wire']=='chat':payload={'model':'claude-sonnet-4-5','stream':stream,'max_tokens':20000,'messages':[{'role':'system','content':'stable '*2000},{'role':'user','content':session}]}
  if case['wire']=='responses':payload={'model':'claude-sonnet-4-5','stream':stream,'max_output_tokens':20000,'input':[{'role':'user','content':session}]}
  status,text,headers=self.request('POST',path,payload,key=case['key']['key'],timeout=60)
  frames=[]
  if stream:
   for line in text.splitlines():
    if line.startswith('data:'):
     try:frames.append(json.loads(line[5:].strip()))
     except ValueError:pass
  else:
   try:frames=[json.loads(text)]
   except ValueError:pass
  lower_headers={k.lower():v for k,v in headers.items()}
  usage=None;request_id=lower_headers.get('x-client-request-id',lower_headers.get('x-request-id',''))
  for f in frames:
   u=f.get('usageMetadata') if case['protocol']=='gemini' else f.get('usage') or (f.get('message') or {}).get('usage') or (f.get('response') or {}).get('usage')
   if u is not None:usage=u
  if status!=200 or usage is None:raise AssertionError(f'{case["id"]}: gateway HTTP {status}, {text[:500]}')
  if case['protocol']=='gemini':
   normalized={'input':usage.get('promptTokenCount',0)-usage.get('cachedContentTokenCount',0),'read':usage.get('cachedContentTokenCount',0),'write':0,'output':usage.get('candidatesTokenCount',0)+usage.get('thoughtsTokenCount',0),'5m':0,'1h':0}
   assert usage.get('totalTokenCount')==sum(normalized[x] for x in ['input','read','output'])+usage.get('toolUsePromptTokenCount',0),(case['id'],'gemini total')
  elif case['wire'] in ['chat','responses']:
   total=usage.get('prompt_tokens',usage.get('input_tokens',0));details=usage.get('prompt_tokens_details',usage.get('input_tokens_details',{}));read=details.get('cached_tokens',0);write=usage.get('cache_creation_input_tokens',0) or details.get('cache_creation_tokens',0) or details.get('cache_write_tokens',0)
   normalized={'input':total-read-write,'read':read,'write':write,'output':usage.get('completion_tokens',usage.get('output_tokens',0)),'5m':0,'1h':0}
  else:normalized={'input':usage.get('input_tokens',0),'read':usage.get('cache_read_input_tokens',0),'write':usage.get('cache_creation_input_tokens',0),'output':usage.get('output_tokens',0),'5m':usage.get('cache_creation',{}).get('ephemeral_5m_input_tokens',0),'1h':usage.get('cache_creation',{}).get('ephemeral_1h_input_tokens',0)}
  record={'case':case['id'],'kind':case['kind'],'protocol':case['protocol'],'variant':case['variant'],'stream':stream,'turn':turn,'status':status,'wire':normalized,'request_id':request_id,'key_id':case['key']['id'],'account_id':case['account']['id'],'group_id':case['group']['id'],'strategy_id':case['strategy']['id'],'strategy_revision':case['strategy']['revision']}
  self.results.append(record)
  return record
 def usage_rows(self,case):
  if case.get('verify_request_ids'):
   rows=[]
   for request_id in case['verify_request_ids']:
    data=self.api('GET',f'/admin/usage?api_key_id={case["key"]["id"]}&group_id={case["group"]["id"]}&request_id='+urllib.parse.quote('client:'+request_id,safe='')+'&page_size=100&exact_total=true')
    rows.extend(data.get('items',data.get('logs',[])) if isinstance(data,dict) else data)
   return rows
  data=self.api('GET',f'/admin/usage?api_key_id={case["key"]["id"]}&group_id={case["group"]["id"]}&page_size=100&exact_total=true')
  rows=data.get('items',data.get('logs',[])) if isinstance(data,dict) else data
  if case.get('verify_subset_ids'):
   wanted={'client:'+request_id for request_id in case['verify_subset_ids']}
   rows=[row for row in rows if row.get('request_id') in wanted]
  return rows
 def update(self,case,patch):
  cfg=copy.deepcopy(case['config']);deep(cfg,patch)
  item=self.api('PUT','/admin/cache-strategies/'+str(case['strategy']['id']),{'name':case['name'],'config':cfg,'expected_revision':case['strategy']['revision']})
  case['strategy']=item;case['config']=item['config']
 def verify_db(self,case,records):
  rows=[]
  for _ in range(30):
   rows=self.usage_rows(case)
   if len(rows)>=len(records):break
   time.sleep(.15)
  assert len(rows)==len(records),(case['id'],'usage row count',len(rows),len(records))
  # Match request IDs when exposed; sequential rows are otherwise matched by ID order.
  rows=sorted(rows,key=lambda r:r['id'])
  by_request={row.get('request_id'):row for row in rows if row.get('request_id')}
  for rec,fallback in zip(records,rows):
   row=by_request.get('client:'+rec['request_id'],by_request.get(rec['request_id'],fallback))
   normalized={'input':row['input_tokens'],'output':row['output_tokens'],'read':row['cache_read_tokens'],'write':row['cache_creation_tokens']}
   raw_recorded={'input':row.get('raw_input_tokens'),'output':row.get('raw_output_tokens'),'read':row.get('raw_cache_read_tokens'),'write':row.get('raw_cache_creation_tokens')}
   assert raw_recorded==case['raw'],(case['id'],'pre-shaping usage mismatch',case['raw'],raw_recorded)
   assert normalized=={k:rec['wire'][k] for k in normalized},(case['id'],'wire/db mismatch',rec['wire'],normalized)
   assert row['account_id']==rec['account_id'] and row['group_id']==case['group']['id'],(case['id'],'wrong routing')
   assert row.get('cache_strategy_id')==case['strategy']['id'],(case['id'],'strategy snapshot')
   for k in ['input_cost','output_cost','cache_read_cost','cache_creation_cost','total_cost','actual_cost']:assert math.isfinite(row[k]) and row[k]>=0,(case['id'],k)
   assert abs(row['total_cost']-sum(row[k] for k in ['input_cost','output_cost','cache_read_cost','cache_creation_cost']))<1e-7,(case['id'],'cost sum')
   assert abs(row['actual_cost']-row['total_cost']*row['rate_multiplier'])<1e-7,(case['id'],'rate')
   assert row['stream']==rec['stream'],(case['id'],'stream/request mismatch',rec['request_id'],row.get('request_id'),rec['stream'],row['stream'],list(by_request))
   if case['wire'] not in ['chat','responses']:
    assert row['cache_creation_5m_tokens']==rec['wire']['5m'] and row['cache_creation_1h_tokens']==rec['wire']['1h'],(case['id'],'TTL wire/db mismatch')
   if normalized['write']==0:assert row['cache_creation_cost']==0 and row['cache_creation_5m_tokens']==0 and row['cache_creation_1h_tokens']==0,(case['id'],'zero writes billed')
   prices=({'input':3e-6,'output':15e-6,'read':.3e-6,'write5':3.75e-6,'write1':6e-6} if case['protocol']=='anthropic' else {'input':1.25e-6,'output':10e-6,'read':.125e-6,'write5':1.25e-6,'write1':1.25e-6})
   expected={'input_cost':normalized['input']*prices['input'],'output_cost':normalized['output']*prices['output'],'cache_read_cost':normalized['read']*prices['read'],'cache_creation_cost':(normalized['write']-row['cache_creation_1h_tokens'])*prices['write5']+row['cache_creation_1h_tokens']*prices['write1']}
   for field,value in expected.items():require(abs(value-row[field])<1e-7,(case['id'],'bucket price mismatch',field,value,row[field]))
   raw=case['raw']
   raw_long=(raw['write']-raw['write']*3//5) if case['protocol']=='anthropic' else 0
   raw_cost=raw['input']*prices['input']+raw['output']*prices['output']+raw['read']*prices['read']+(raw['write']-raw_long)*prices['write5']+raw_long*prices['write1']
   assert row.get('raw_total_cost') is not None,(case['id'],'missing raw cost')
   assert abs(row['raw_total_cost']-raw_cost)<1e-7,(case['id'],'raw cost mismatch',row['raw_total_cost'],raw_cost)
   expected_cost=sum(expected.values())
   assert abs(expected_cost-row['total_cost'])<1e-7,(case['id'],'price mismatch',expected_cost,row['total_cost'])
   rec['db']={k:row[k] for k in ['id','input_tokens','output_tokens','cache_read_tokens','cache_creation_tokens','cache_creation_5m_tokens','cache_creation_1h_tokens','total_cost','actual_cost','rate_multiplier','cache_strategy_id','raw_total_cost','raw_input_tokens','raw_output_tokens','raw_cache_read_tokens','raw_cache_creation_tokens']}
  return rows
 def cleanup(self):
  failures=[]
  for kind,id in sorted(reversed(self.resources),key=lambda item: 0 if item[0]=="key" else 1):
   path={'key':'/keys/','account':'/admin/accounts/','group':'/admin/groups/','strategy':'/admin/cache-strategies/','user':'/admin/users/'}[kind]+str(id)
   try:self.api('DELETE',path,user=kind=='key')
   except Exception as e:failures.append(str(e))
  return failures
 def finish(self,errors):
  errors.extend(self.failures)
  cleanup=self.cleanup();self.server.shutdown()
  report={'run':self.prefix,'requests':len(self.results),'gateway_http_requests':len(self.http),'mock_calls':len(CALLS),'errors':errors,'cleanup_errors':cleanup,'results':self.results,'http':self.http,'checkpoints':self.checkpoints}
  Path(self.args.report).write_text(json.dumps(report,indent=2,ensure_ascii=False))
  print(json.dumps({'run':self.prefix,'requests':len(self.results),'mock_calls':len(CALLS),'failures':len(errors),'cleanup_failures':len(cleanup),'report':self.args.report}))
  if errors or cleanup:raise SystemExit(1)

def deep(dst,src):
 for k,v in src.items():
  if isinstance(v,dict) and isinstance(dst.get(k),dict):deep(dst[k],v)
  else:dst[k]=copy.deepcopy(v)
 return dst

def scenario(runner,protocol,kind,variant,*args,**kwargs):
 try:return scenario_impl(runner,protocol,kind,variant,*args,**kwargs)
 except Exception as error:
  detail=repr(error);runner.failures.append(detail);print('FAIL',protocol,kind,variant,detail,flush=True)
  return None,[]

def scenario_impl(runner,protocol,kind,variant,patch=None,raw=None,checks=None,turns=2,mock='normal',wire=None,custom_body=None):
 case=runner.setup(protocol,kind,variant,patch,raw,mock,wire)
 records=[]
 for stream in [False,True]:
  session=case['id']+str(stream)
  for turn in range(turns):
   body=custom_body(runner,case,stream,session,turn) if custom_body else None
   rec=runner.forward(case,stream,session,turn,body=body);records.append(rec)
   if checks:checks(case,rec,turn)
 runner.verify_db(case,records)
 print('PASS',protocol,kind,variant,len(records),'requests wire=db cost=expected',flush=True)
 return case,records

def require(condition,detail):
 if not condition:raise AssertionError(detail)

def common_check(case,rec,turn):
 u=rec['wire'];require(all(v>=0 for v in u.values()),(case['id'],'negative usage'))
 if case['protocol']=='gemini':require(u['write']==0,(case['id'],'Gemini write'))
 if case['kind']!='disabled':require(sum(u[k] for k in ['input','read','write'])<= (200000 if case['protocol']=='anthropic' else 1048576),(case['id'],'model cap'))

def full_matrix(r):
 for protocol in ['anthropic','gemini']:
  for kind in KINDS:
   if r.args.filter and protocol+'/'+kind not in r.args.filter.split(','):continue
   scenario(r,protocol,kind,'default',checks=common_check,turns=3)
   raw={'input':8000,'read':2000,'write':1000 if protocol=='anthropic' else 0,'output':1200}
   scenario(r,protocol,kind,'raw-evidence',raw=raw,checks=common_check)
   if kind=='disabled':continue
   scenario(r,protocol,kind,'usage-off',patch={'usage':{'enabled':False}},raw=raw,checks=lambda c,x,t:require({k:x['wire'][k] for k in raw}==raw,(c['id'],'usage disabled changed raw')))
   if kind=='usage_multiplier':
    for factor in [0,.5,1.5,2]:
     patch={'usage_multiplier':{k:{'threshold_tokens':0,'multiplier':factor} for k in ['input','output','cache_read','cache_creation']}}
     scenario(r,protocol,kind,'factor-'+str(factor),patch,raw,checks=lambda c,x,t,f=factor:require(all(x['wire'][k]==int(math.floor(v*f+.5)) for k,v in raw.items()),(c['id'],'wrong multiplier',x['wire'])))
    for output in [999,1000,1001]:
     special={**raw,'output':output}
     scenario(r,protocol,kind,'threshold-'+str(output),{'usage_multiplier':{'output':{'threshold_tokens':1000,'multiplier':.5}}},special,checks=lambda c,x,t,n=output:require(x['wire']['output']==(n if n<=1000 else 501),(c['id'],'threshold')))
    scenario(r,protocol,kind,'all-limits',{'usage_multiplier':{k:{'threshold_tokens':0,'multiplier':1000} for k in ['input','output','cache_read','cache_creation']},'limits':{'max_input_tokens':5000,'max_output_tokens':100}},raw,checks=lambda c,x,t:require(sum(x['wire'][k] for k in ['input','read','write'])<=5000 and x['wire']['output']<=100,(c['id'],'limits')))
    if protocol=='gemini':scenario(r,protocol,kind,'partial-multiframe',{'usage_multiplier':{'output':{'threshold_tokens':1000,'multiplier':.5}}},raw,mock='multiframe',checks=lambda c,x,t:require(x['wire']['output']==(501 if x['stream'] else 600) and x['wire']['input']==raw['input'],(c['id'],'multiframe')))
   else:
    patch={'limits':{'max_input_tokens':5000,'max_output_tokens':100},'preserve_upstream_cache_usage':False}
    scenario(r,protocol,kind,'final-limits',patch,{'input':1500000,'output':200000,'read':0,'write':0},checks=lambda c,x,t:require(sum(x['wire'][k] for k in ['input','read','write'])<=5000 and x['wire']['output']<=100,(c['id'],'limits')))
    scenario(r,protocol,kind,'no-session',{'allow_derived_session':False},checks=lambda c,x,t:require(x['wire']['read']==0,(c['id'],'no-session cache')),custom_body=lambda rr,c,st,se,t:without_session(rr.body(c,st,se,t)))
    scenario(r,protocol,kind,'skip-json',{'usage':{'skip_non_stream_usage_projection':True}},raw,checks=lambda c,x,t:require(x['stream'] or all(x['wire'][k]==v for k,v in raw.items()),(c['id'],'skip JSON')))
    if kind=='prefix_tool_plus':
     scenario(r,protocol,kind,'plus-raw',{'tool_plus':{'non_stream':{'mode':'raw'}}},raw,checks=lambda c,x,t:require(x['stream'] or all(x['wire'][k]==v for k,v in raw.items()),(c['id'],'plus raw')))
     scenario(r,protocol,kind,'plus-small-off',{'tool_plus':{'small_request':{'enabled':True,'max_input_tokens':4000000,'disable_cache':True}}},checks=lambda c,x,t:require(x['wire']['read']==0 and x['wire']['write']==0,(c['id'],'small disable')))
    if kind=='stable_segment_cache':
     scenario(r,protocol,kind,'stable-input-band',{'uncached_input_min_tokens':0,'uncached_input_max_tokens':0,'stable_segment':{'reported_input_min_tokens':64,'reported_input_max_tokens':96},'cold_request_guard':False},checks=lambda c,x,t:require(t==0 or 64<=x['wire']['input']<=96,(c['id'],'stable band',x['wire'])))
    scenario(r,protocol,kind,'zero-ratios',{'usage_ratio':0,'read_ratio':0,'creation_ratio':0,'preserve_upstream_cache_usage':False},checks=lambda c,x,t:require(x['wire']['read']==0 and x['wire']['write']==0,(c['id'],'zero ratios')))
    scenario(r,protocol,kind,'read-output-caps',{'preserve_upstream_cache_usage':False,'usage':{'final_cache_read_max_tokens':500,'final_cache_read_jitter_min_tokens':10,'final_cache_read_jitter_max_tokens':20,'final_output_max_tokens':400,'final_output_jitter_min_tokens':10,'final_output_jitter_max_tokens':20}},checks=lambda c,x,t:require(x['wire']['read']<=500 and x['wire']['output']<=400,(c['id'],'field caps')))
    if protocol=='anthropic':
     scenario(r,protocol,kind,'strict-ineligible',{'usage':{'strict_cache_eligibility_stream':True,'strict_cache_eligibility_non_stream':True}},raw,checks=lambda c,x,t:require(all(x['wire'][k]==v for k,v in raw.items()),(c['id'],'strict passthrough')),custom_body=lambda rr,c,st,se,t:tiny_body(rr,c,st,se,t))
     scenario(r,protocol,kind,'creation-frequency',{'creation_control':{'enabled':True,'min_creation_interval_seconds':60,'max_creation_tokens_per_event':500,'max_creation_tokens_per_window':1000},'preserve_upstream_cache_usage':False},checks=lambda c,x,t:require(x['wire']['write']<=500 and (t==0 or x['wire']['write']==0),(c['id'],'creation frequency')),turns=3)
    if kind in ['prefix','tool_aware','bounded_high_cache']:
     scenario(r,protocol,kind,'hit-shaping',{'hit_shaping':{'enabled':True,'target_hit_ratio':.8,'ratio_jitter':0,'min_cost_ratio':0,'min_cache_read_tokens':0,'min_cache_creation_tokens':0},'uncached_input_min_tokens':0,'uncached_input_max_tokens':0,'cold_request_guard':True,'preserve_upstream_cache_usage':False},checks=lambda c,x,t:require((t>0 and x['wire']['read']>0) or (t==0 and x['wire']['read']==0),(c['id'],'cold/hot target')))
    if r.args.extended:
     for probe in [failure_retry,ttl_expiry]:
      try:probe(r,protocol,kind)
      except Exception as e:r.failures.append(str(e));print('FAIL',str(e),flush=True)

  # Cross-protocol policy is a no-op.
  if r.args.filter:continue
  case=r.setup(protocol,'usage_multiplier','cross-protocol',{'usage_multiplier':{'input':{'multiplier':2},'output':{'multiplier':2}}})
  other='gemini' if protocol=='anthropic' else 'anthropic'
  r.update(case,{'protocol':other})
  records=[r.forward(case,False,case['id'])];r.verify_db(case,records)
  require(records[0]['wire']['input']==10000 and records[0]['wire']['output']==1200,(case['id'],'cross protocol shaped'))
  print('PASS',protocol,'cross-protocol bypass',flush=True)

def tiny_body(r,c,stream,session,turn):
 b=r.body(c,stream,session,turn);b.pop('system',None);b['messages']=[{'role':'user','content':'hi'}];return b

def failure_retry(r,protocol,kind):
 case=r.setup(protocol,kind,'incomplete-retry',{'preserve_upstream_cache_usage':False},mock='incomplete')
 # An incomplete stream must not establish a prefix for the next successful call.
 session=case['id'];body=r.body(case,True,session)
 path='/v1/messages' if protocol=='anthropic' else '/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse'
 status,text,headers=r.request('POST',path,body,key=case['key']['key'])
 require(status in [200,502],(case['id'],'incomplete status',status))
 with LOCK:CASES[case['id']]['mock']='normal'
 rec=r.forward(case,True,session,1)
 require(rec['wire']['read']==0,(case['id'],'failed request committed cache'))
 # Failed requests can retain partial usage, so compare only the successful row by token values.
 rows=[];matched=[]
 for _ in range(30):
  rows=r.usage_rows(case)
  matched=[row for row in rows if row['input_tokens']==rec['wire']['input'] and row['output_tokens']==rec['wire']['output'] and row['cache_read_tokens']==0]
  if matched:break
  time.sleep(.15)
 require(bool(matched),(case['id'],'successful retry missing usage'))
 rec['db']={k:matched[-1][k] for k in ['id','input_tokens','output_tokens','cache_read_tokens','cache_creation_tokens','total_cost','actual_cost']}

 print('PASS',protocol,kind,'incomplete stream does not warm cache',flush=True)

def ttl_expiry(r,protocol,kind):
 case=r.setup(protocol,kind,'ttl-expiry',{'state_ttl_seconds':1,'default_ttl_seconds':1,'preserve_upstream_cache_usage':False})
 session=case['id'];records=[]
 records.append(r.forward(case,False,session,0));records.append(r.forward(case,False,session,1))
 require(records[0]['wire']['read']==0 and records[1]['wire']['read']>0,(case['id'],'TTL warm up'))
 time.sleep(1.1)
 records.append(r.forward(case,False,session,2));require(records[2]['wire']['read']==0,(case['id'],'TTL did not expire'))
 r.verify_db(case,records);print('PASS',protocol,kind,'TTL expires through gateway',flush=True)

def extra_matrix(r):
 if r.args.scope_only:return scope_matrix(r)
 phases=r.args.phases.split(',')
 if 'all' in phases or 'compatibility' in phases:compatibility_matrix(r)
 if 'all' in phases or 'explicit' in phases:explicit_matrix(r)
 if 'all' in phases or 'lifecycle' in phases:
  for protocol in ['anthropic','gemini']:
   for kind in KINDS:guard(r,lifecycle_case,protocol,kind)
 if 'all' in phases or 'retry' in phases:
  for protocol in ['anthropic','gemini']:
   for mode in ['http_once','http_error']:guard(r,retry_case,protocol,mode)
 if not r.args.skip_scope and ('all' in phases or 'scope' in phases):scope_matrix(r)
 if 'all' in phases or 'concurrent' in phases:
  for protocol in ['anthropic','gemini']:guard(r,concurrent_case,protocol)
 if 'all' in phases or 'balance' in phases:guard(r,balance_case)
 if 'all' in phases or 'edges' in phases:multiplier_edges(r)

def guard(r,probe,*args):
 try:probe(r,*args)
 except Exception as error:
  r.failures.append(repr(error));print('FAIL',probe.__name__,*args,repr(error),flush=True)

def compatibility_matrix(r):
 # Protocol compatibility routes reuse the same Anthropic policy and preserve three-bucket totals.
 for kind in KINDS:
  for wire in ['chat','responses']:
   scenario(r,'anthropic',kind,'compat-'+wire,raw={'input':8000,'output':1200,'read':2000,'write':0},wire=wire,checks=common_check)
def explicit_matrix(r):
 # Explicit resources must be created through the same owner/key API before they can be referenced.
 for kind in KINDS:
  patch={'usage_multiplier':{'cache_read':{'threshold_tokens':0,'multiplier':.5}}} if kind=='usage_multiplier' else {}
  case=r.setup('gemini',kind,'explicit-resource',patch,{'input':8000,'read':2000,'write':0,'output':1200})
  code,text,headers=r.request('POST','/v1beta/cachedContents',{'model':'models/gemini-2.5-pro','ttl':'300s','contents':[{'role':'user','parts':[{'text':'cached '*2000}]}]},key=case['key']['key'])
  require(code==200,(case['id'],'create native cache failed',code,text[:300]))
  resource=json.loads(text)['name'];records=[]
  try:
   for stream in [False,True]:
    body=r.body(case,stream,case['id']);body['cachedContent']=resource
    rec=r.forward(case,stream,case['id'],body=body);records.append(rec)
    require(rec['wire']['read']==(1000 if kind=='usage_multiplier' else 2000),(case['id'],'explicit cached read'))
   rows=[]
   for _ in range(30):
    rows=r.usage_rows(case)
    if len(rows)>=3:break
    time.sleep(.1)
   generations=[row for row in rows if row.get('request_type')!='cache_create' and row['output_tokens']>0]
   require(len(generations)==2,(case['id'],'explicit generation record count',len(rows)))
   creates=[row for row in rows if row['output_tokens']==0]
   require(len(creates)==1 and creates[0]['input_tokens']==2000,(case['id'],'explicit creation input'))
   creation=creates[0];expected_storage=2000*300/3600*4.5e-6
   require(abs(creation['total_cost']-(2000*1.25e-6+expected_storage))<1e-7,(case['id'],'explicit creation/storage cost',creation['total_cost']))
   r.checkpoints.append({'case':case['id'],'create_input_tokens':2000,'create_total_cost':creation['total_cost'],'expected_storage_cost':expected_storage,'passed':True})
   for rec,row in zip(records,sorted(generations,key=lambda row:row['id'])):
    require(rec['wire']['input']==row['input_tokens'] and rec['wire']['read']==row['cache_read_tokens'] and rec['wire']['output']==row['output_tokens'],(case['id'],'explicit usage/db'))
    expected=rec['wire']['input']*1.25e-6+rec['wire']['read']*.125e-6+rec['wire']['output']*10e-6
    require(abs(expected-row['total_cost'])<1e-7,(case['id'],'explicit generation price',expected,row['total_cost']))
    rec['db']={k:row[k] for k in ['id','input_tokens','output_tokens','cache_read_tokens','cache_creation_tokens','total_cost','actual_cost']}
   print('PASS gemini',kind,'owned cachedContents create/generate/delete',flush=True)
  finally:
   code,text,headers=r.request('DELETE','/v1beta/'+resource,key=case['key']['key'])
   require(code==200,(case['id'],'explicit delete',code))
def lifecycle_case(r,protocol,kind):
 case=r.setup(protocol,kind,'strategy-disabled',{'usage_multiplier':{'output':{'multiplier':2}}})
 item=r.api('POST','/admin/cache-strategies/'+str(case['strategy']['id'])+'/disable',{})
 case['strategy']=item
 records=[r.forward(case,False,case['id']),r.forward(case,True,case['id'])]
 require(all(x['wire']['output']==1200 for x in records),(case['id'],'disabled strategy projected'))
 r.verify_db(case,records)
 case=r.setup(protocol,kind,'http-failure-retry',mock='http_400')
 before=len(CALLS);path='/v1/messages' if protocol=='anthropic' else '/v1beta/models/gemini-2.5-pro:generateContent'
 code,body,headers=r.request('POST',path,r.body(case,False,case['id']),key=case['key']['key'])
 require(code==400 and len(CALLS)>before,(case['id'],'failure did not reach mock',code))
 time.sleep(.2)
 require(not r.usage_rows(case),(case['id'],'HTTP failure billed before retry'))
 with LOCK:CASES[case['id']]['mock']='normal'
 r.api('POST','/admin/accounts/'+str(case['account']['id'])+'/clear-error',{})
 r.api('POST','/admin/accounts/'+str(case['account']['id'])+'/clear-rate-limit',{})
 rec=r.forward(case,False,case['id'],1);require(rec['wire']['read']==0,(case['id'],'HTTP failure warmed cache'))
 rows=[]
 for _ in range(30):
  rows=r.usage_rows(case)
  if rows:break
  time.sleep(.15)
 require(len(rows)==1,(case['id'],'HTTP failure created billable usage',len(rows)))
 r.verify_db(case,[rec])
 print('PASS',protocol,kind,'disabled strategy and HTTP error retry',flush=True)

def retry_case(r,protocol,mode):
 case=r.setup(protocol,'prefix',mode,mock=mode)
 path='/v1/messages' if protocol=='anthropic' else '/v1beta/models/gemini-2.5-pro:generateContent'
 before=len(CALLS)
 if mode=='http_once':
  rec=r.forward(case,False,case['id']);require(len(CALLS)-before>=2,(case['id'],'503 was not retried'))
  require(rec['wire']['read']==0,(case['id'],'failed attempt warmed cache'))
  r.verify_db(case,[rec])
 else:
  code,text,headers=r.request('POST',path,r.body(case,False,case['id']),key=case['key']['key'],timeout=90)
  require(code>=400 and len(CALLS)-before>=2,(case['id'],'503 retry exhaustion',code))
  require(not r.usage_rows(case),(case['id'],'retry exhaustion billed'))
 r.checkpoints.append({'case':case['id'],'mock_attempts':len(CALLS)-before,'passed':True})
 print('PASS',protocol,mode,'retry attempts',len(CALLS)-before,'usage rows correct',flush=True)

def concurrent_case(r,protocol):
 case=r.setup(protocol,'usage_multiplier','concurrent',{'usage_multiplier':{'output':{'threshold_tokens':1000,'multiplier':1.5}}})
 with ThreadPoolExecutor(max_workers=6) as pool:records=list(pool.map(lambda n:r.forward(case,n%2==0,case['id']+str(n)),range(12)))
 rows=r.verify_db(case,records)
 require(len(rows)==12,(case['id'],'concurrent row count'))
 for row in rows:require(row['output_tokens']==1800 and row['account_id']==case['account']['id'],(case['id'],'concurrency usage'))
 require(all(x['wire']['output']==1800 for x in records),(case['id'],'concurrency wire'))
 print('PASS',protocol,'12 concurrent sessions wire/db output=1800',flush=True)

def balance_case(r):
 before=r.api('GET','/admin/users/'+str(r.user_id))['balance']
 billed=0
 for protocol in ['anthropic','gemini']:
  raw={'input':2000,'read':4000,'write':1000 if protocol=='anthropic' else 0,'output':1200}
  case=r.setup(protocol,'usage_multiplier','balance-debit',{'usage_multiplier':{'input':{'multiplier':.5},'cache_read':{'multiplier':2},'cache_creation':{'multiplier':.5},'output':{'threshold_tokens':1000,'multiplier':1.5}}},raw)
  records=[r.forward(case,False,case['id']),r.forward(case,True,case['id'])]
  rows=r.verify_db(case,records);billed+=sum(row['actual_cost'] for row in rows)
  require(all(rec['wire']=={'input':1000,'read':8000,'write':500 if protocol=='anthropic' else 0,'output':1800,'5m':300 if protocol=='anthropic' else 0,'1h':200 if protocol=='anthropic' else 0} for rec in records),(case['id'],'balance parameter results'))
 after=r.api('GET','/admin/users/'+str(r.user_id))['balance']
 require(abs(before-after-billed)<1e-7,('balance debit mismatch',before-after,billed))
 r.checkpoints.append({'case':'balance-debit','expected_cost':billed,'balance_change':before-after,'passed':True})
 print('PASS balance debit equals persisted actual_cost',billed,flush=True)

def multiplier_edges(r):
 for protocol in ['anthropic','gemini']:
  fields={'input':'input','output':'output','cache_read':'read'}
  if protocol=='anthropic':fields['cache_creation']='write'
  for field,bucket in fields.items():
   for n in [999,1000,1001]:
    raw={'input':2000,'read':2000,'write':1000 if protocol=='anthropic' else 0,'output':1200};raw[bucket]=n
    scenario(r,protocol,'usage_multiplier',f'{field}-edge-{n}',{'usage_multiplier':{field:{'threshold_tokens':1000,'multiplier':.5}}},raw,turns=1,checks=lambda c,x,t,k=bucket,v=n:require(x['wire'][k]==(v if v<=1000 else 501),(c['id'],'bucket threshold',k,x['wire'])))
  def requested_cap(rr,c,stream,session,turn):return rr.body(c,stream,session,turn,maxout=77)
  scenario(r,protocol,'usage_multiplier','request-output-cap',{'usage_multiplier':{'output':{'multiplier':1000}},'limits':{'max_output_tokens':100}},turns=1,custom_body=requested_cap,checks=lambda c,x,t:require(x['wire']['output']==77,(c['id'],'request output cap')))
  def huge_request(rr,c,stream,session,turn):return rr.body(c,stream,session,turn,maxout=1000000)
  def model_cap(c,x,t):
   u=x['wire'];context=200000 if c['protocol']=='anthropic' else 1048576
   require(sum(u[k] for k in ['input','read','write'])<=context,(c['id'],'model input total'))
   require(u['output']<= (64000 if c['protocol']=='anthropic' else 65535),(c['id'],'model output limit'))
  scenario(r,protocol,'usage_multiplier','model-context-cap',{'usage_multiplier':{f:{'multiplier':1000} for f in fields},'limits':{'max_input_tokens':0,'max_output_tokens':0}},raw={'input':1500000,'read':300000,'write':200000 if protocol=='anthropic' else 0,'output':200000},turns=1,custom_body=huge_request,checks=model_cap)

def scope_matrix(r):
 # Scope isolation and revision invalidation through actual account scheduling.
 for protocol in ([] if r.args.skip_scope else ['anthropic','gemini']):
  for kind in ['prefix','tool_aware','stable_segment_cache','prefix_tool_plus','bounded_high_cache']:
   for scope in ['group_session','group_account_session']:
    guard(r,scope_case,protocol,kind,scope)

def scope_case(r,protocol,kind,scope):
 case=r.setup(protocol,kind,'scope-'+scope,{'scope_mode':scope,'preserve_upstream_cache_usage':False})
 session=case['id'];records=[r.forward(case,False,session,0)]
 first_account=case['account']
 second=r.api('POST','/admin/accounts',{'name':case['name']+'_second','platform':protocol,'type':'apikey','credentials':{'api_key':'mock-not-a-real-key','explicit_cache_upstream':True,'base_url':f'http://127.0.0.1:{r.args.mock_port}/{case["id"]}/{protocol}'},'group_ids':[case['group']['id']],'concurrency':30,'priority':1,'status':'active','schedulable':True,'upstream_billing_probe_enabled':False,'upstream_billing_group_sync_enabled':False})
 r.resources.append(('account',second['id']));r.api('PUT','/admin/accounts/'+str(first_account['id']),{'status':'inactive'})
 case['account']=second
 for attempt in range(12):
  try:records.append(r.forward(case,False,session,1));break
  except AssertionError as error:
   if 'HTTP 503' not in str(error) or attempt==11:raise
   time.sleep(1)

 require((records[1]['wire']['read']>0)==(scope=='group_session'),(case['id'],'account scope isolation',records[1]['wire']))
 # Revision changes invalidate the previous strategy state even in the same session.
 r.update(case,{})
 records.append(r.forward(case,False,session,2));require(records[2]['wire']['read']==0,(case['id'],'revision retained cache'))
 rows=[]
 for _ in range(30):
  rows=r.usage_rows(case)
  if len(rows)==3:break
  time.sleep(.1)
 require(len(rows)==3,(case['id'],'scope rows'))
 require({x['account_id'] for x in rows}=={first_account['id'],second['id']},(case['id'],'account switching did not occur'))
 r.verify_db(case,records)
 print('PASS',protocol,kind,scope,'account switch and revision invalidation',flush=True)

def without_session(body):
 body.pop('metadata',None)
 return body

def main():
 parser=argparse.ArgumentParser();parser.add_argument('--base',default='http://127.0.0.1:48788');parser.add_argument('--mock-port',type=int,default=48791);parser.add_argument('--report',default='/tmp/cache-gateway-e2e.json');parser.add_argument('--probe',action='store_true');parser.add_argument('--full',action='store_true');parser.add_argument('--filter',default='');parser.add_argument('--extended',action='store_true');parser.add_argument('--extra-only',action='store_true');parser.add_argument('--skip-scope',action='store_true');parser.add_argument('--scope-only',action='store_true');parser.add_argument('--phases',default='all',help='extra phases: compatibility,explicit,lifecycle,retry,scope,concurrent,balance,edges; core skips extra phases');args=parser.parse_args()
 runner=Runner(args);errors=[]
 try:
  runner.bootstrap()
  if args.extra_only:extra_matrix(runner)
  elif args.full:
   full_matrix(runner)
   if args.extended and not args.filter:extra_matrix(runner)
  else:
   for protocol in ['anthropic','gemini']:
    for kind in (['usage_multiplier'] if args.probe else KINDS):
     case=runner.setup(protocol,kind,'baseline')
     records=[]
     for stream in [False,True]:
      session=case['id']+str(stream)
      for turn in range(2):records.append(runner.forward(case,stream,session,turn))
     runner.verify_db(case,records)
     print('PASS',protocol,kind,'cold/hot JSON/SSE wire=db',flush=True)
 except Exception as e:errors.append(str(e));print('FAIL',str(e),flush=True)
 finally:runner.finish(errors)
if __name__=='__main__':main()
