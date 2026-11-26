#!/usr/bin/env python3
"""Build a coverage receipt; original failed reports remain unchanged and linked."""
import csv,json,hashlib,re,copy
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
OUT=ROOT/'.cachetest/cache-parameter-acceptance-20261127';OUT.mkdir(parents=True,exist_ok=True)
base_path=Path('/tmp/cache-parameter-all-final.json');correction_path=Path('/tmp/cache-parameter-combined-correction.json')
upper_path=Path('/tmp/cache-parameter-upper-clean.json');sem_path=Path('/tmp/cache-parameter-semantics-final.json');ttl_path=Path('/tmp/cache-parameter-ttl-correction.json');last_path=Path('/tmp/cache-parameter-last-semantics.json');contract_path=Path('/tmp/cache-parameter-contracts-clean.json');integration_path=Path('/tmp/cache-parameter-runtime-integration-final.json')
paths=[base_path,correction_path,upper_path,sem_path,ttl_path,last_path,contract_path,integration_path]
data=[json.loads(p.read_text())for p in paths]
base,correction,upper,sem,ttl,last,contract,integration=data
assert not any(d.get('errors') for d in [correction,upper,ttl,last,contract,integration])
assert not any(d.get('cleanup_errors')for d in data)
key=lambda x:(x['protocol'],x['strategy'],x['path'])
fields={key(e):copy.deepcopy(e)for e in base['coverage']}
for e in correction['coverage']:fields[key(e)]=copy.deepcopy(e)
assert len(fields)==2632 and all(e['passed']for e in fields.values())
assert len(upper['coverage'])==1960 and all(e['passed']for e in upper['coverage'])
semkey=lambda x:(x['protocol'],x['strategy'],x['probe'])
semantics={semkey(e):e for e in sem['coverage']}
for d in [ttl,last]:
 for e in d['coverage']:semantics[semkey(e)]=e
assert all(e['passed']for e in semantics.values())
structs={}
for name in ['cache_strategy.go','cache_shaping_config.go','cache_usage_multiplier.go']:
 source=(ROOT/'backend/internal/service'/name).read_text()
 for name,body in re.findall(r'type (\w+) struct \{(.*?)\n\}',source,re.S):
  structs[name]=[(x.group(2),x.group(3))for x in re.finditer(r'^\s*(\w+)\s+([\w*\[\]]+(?:\[string\]\w+)?)\s+`json:"([^",]+)',body,re.M)]
def schema(name,prefix=''):
 for typ,key in structs[name]:
  path=prefix+'.'+key if prefix else key
  if typ in structs:yield from schema(typ,path)
  else:yield path
expected=set(schema('CacheStrategyConfig'))
assert len(expected)==188
for protocol in ['anthropic','gemini']:
 for kind in ['disabled','prefix','tool_aware','stable_segment_cache','prefix_tool_plus','bounded_high_cache','usage_multiplier']:
  assert {p for pr,k,p in fields if (pr,k)==(protocol,kind)}==expected
receipts=[]
for path,d in zip(paths,data):
 target=OUT/path.name;target.write_bytes(path.read_bytes())
 receipts.append({'path':str(target),'sha256':hashlib.sha256(path.read_bytes()).hexdigest(),'run':d.get('run'),'http_requests':d.get('http_requests',d.get('gateway_http_requests',0)),'successful_generations':d.get('success_generations',d.get('requests',0)),'original_errors':len(d.get('errors',[]))})
rows=[]
for (protocol,kind,path),entry in fields.items():
 related=[e['probe']for e in semantics.values()if e['protocol']==protocol and e['strategy']==kind and path in e['paths']]
 role='公共参数'
 if kind=='disabled':role='禁用策略：归一化后不投影'
 elif path.startswith('usage_multiplier.') and kind!='usage_multiplier':role='其他模式保留字段，不应用直接倍率'
 elif kind=='usage_multiplier' and path not in ['protocol','kind','usage.enabled','limits.max_input_tokens','limits.max_output_tokens'] and not path.startswith('usage_multiplier.'):role='直接倍率独立：保留旧字段，不参与时机或采样'
 elif path.startswith('tool_plus.') and kind!='prefix_tool_plus':role='Plus专属，当前策略不应用'
 elif path.startswith('stable_segment.') and kind!='stable_segment_cache':role='stable专属，当前策略不应用'
 elif protocol=='anthropic' and kind=='prefix_tool_plus' and path in ['usage.input.mode','usage.output_uplift_enabled','usage.output_uplift_min_tokens','usage.output_uplift_percent']:role='Plus输入/输出使用独立profile，不叠加旧input模式或输出放大'
 elif protocol=='gemini' and (path.startswith('creation_control.') or path.startswith('usage.cache_creation.') or path.startswith('usage.final_cache_creation') or path in ['forced_ttl_tier','creation_ratio','hit_shaping.min_cache_creation_tokens','hit_shaping.min_creation_ratio']):role='Gemini生成无缓存写：归一化为不写'
 elif protocol=='gemini' and path in ['breakpoint_mode','cap_jitter_min_tokens','cap_jitter_max_tokens','hour_ttl_seconds','trust_upstream_ttl_tier','stable_segment.honor_explicit_cache_control','hit_shaping.min_cost_ratio','hit_shaping.read_aware_cost_floor','hit_shaping.cost_floor_top_up_input'] or protocol=='gemini' and path.startswith('tool_plus.') and path.endswith('.min_cost_ratio'):role='Gemini协议保留兼容字段，三桶路径不执行此Anthropic语义'
 elif path in ['usage.output.move_delta_to_cache_read','usage.cache_read.move_delta_to_cache_read','usage.cache_creation.move_delta_to_cache_read']:role='共享结构兼容字段，仅input桶支持差额转读'
 rows.append({'protocol':protocol,'strategy':kind,'path':path,'status':'PASS','runtime_role':role,'tested_values':json.dumps([x['value']for x in entry['trials']],ensure_ascii=False),'invalid_trials':len(entry.get('invalid',[])),'upper_bound_tested':any(key(e)==(protocol,kind,path)for e in upper['coverage']),'semantic_probes':';'.join(related),'request_ids':';'.join(x for t in entry['trials'] for x in t.get('request_ids',[]))})
with (OUT/'coverage.csv').open('w',newline='')as f:
 w=csv.DictWriter(f,fieldnames=list(rows[0]));w.writeheader();w.writerows(rows)
summary={'date':'2026-11-27','schema_leaves':188,'strategy_parameter_positions':len(rows),'passed_positions':len(rows),'numeric_upper_positions':len(upper['coverage']),'invalid_value_trials':sum(len(x.get('invalid',[]))for x in fields.values()),'omission_null_crossfield_api_trials':len(contract['records']),'semantic_probes':len(semantics),'unresolved_failures':0,'receipts':receipts,'notes':['Original base report has 10 fixture errors in disabled→active mode switching; corrected API requests are separately retained.','Original semantic report has one explicit 5m/default 1s fixture error; dedicated auto-TTL correction passed.','Upper and contract final reports contain no errors.','Coverage receipt merges only explicit passed replacement entries; original reports are not rewritten.']}
(OUT/'acceptance.json').write_text(json.dumps(summary,ensure_ascii=False,indent=2));print(json.dumps(summary,ensure_ascii=False,indent=2))
