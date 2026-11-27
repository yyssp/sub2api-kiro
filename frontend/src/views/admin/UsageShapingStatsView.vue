<template>
  <AppLayout>
    <div class="space-y-5">
      <div class="card flex flex-wrap items-center gap-3 p-4">
        <div class="flex rounded-lg bg-gray-100 p-1 dark:bg-dark-800" role="group" :aria-label="t('admin.usageShaping.breakdown')">
          <button v-for="value in dimensions" :key="value" type="button" class="rounded-md px-4 py-2 text-sm" :aria-pressed="dimension === value" :class="dimension === value ? 'bg-white font-medium shadow-sm dark:bg-dark-600' : 'text-gray-500'" @click="dimension = value">{{ t(`admin.usageShaping.${value}`) }}</button>
        </div>
        <DateRangePicker v-model:start-date="startDate" v-model:end-date="endDate" @change="load" />
        <Select v-model="selected" :options="entityOptions" class="w-full sm:w-52" :aria-label="t(`admin.usageShaping.${dimension}`)" />
        <button v-if="selected" type="button" class="btn btn-ghost" @click="selected = ''">{{ t('admin.usageShaping.clear') }}</button>
        <input v-model="search" type="search" class="input w-full sm:ml-auto sm:w-60" :placeholder="t('admin.usageShaping.search')" />
        <button type="button" class="btn btn-secondary" :disabled="loading" @click="load">{{ t('admin.usageShaping.refresh') }}</button>
      </div>
      <div v-if="error" role="alert" class="rounded-lg bg-red-50 p-4 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ error }}</div>
      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-5" :aria-busy="loading">
        <div class="card p-4">
          <p class="text-xs text-gray-500" :title="t('admin.usageShaping.requestsHelp')">{{ t('admin.usageShaping.requests') }}</p>
          <p class="mt-3 text-2xl font-semibold tabular-nums">{{ loading ? '—' : totals.requests.toLocaleString() }}</p>
          <p class="mt-2 text-xs text-gray-500">{{ t('admin.usageShaping.comparable') }} {{ totals.comparable_requests.toLocaleString() }} · {{ coverage }}%</p>
        </div>
        <div v-for="stage in ['before', 'after'] as const" :key="stage" class="card p-4">
          <p class="text-xs text-gray-500">{{ t(`admin.usageShaping.${stage}Cost`) }} · USD</p>
          <p class="mt-3 text-2xl font-semibold tabular-nums">{{ hasComparable && !loading ? money(stage === 'before' ? totals.before_cost : totals.after_cost) : '—' }}</p>
          <p class="mt-2 text-xs text-gray-500">{{ t('admin.usageShaping.comparable') }} {{ totals.comparable_requests.toLocaleString() }}</p>
        </div>
        <div class="card p-4">
          <p class="text-xs text-gray-500">{{ t('admin.usageShaping.costDelta') }}</p>
          <p class="mt-3 text-2xl font-semibold tabular-nums" :class="tone(totals.after_cost - totals.before_cost)">{{ hasComparable && !loading ? signedMoney(totals.after_cost - totals.before_cost) : '—' }}</p>
          <p class="mt-2 text-xs text-gray-500"><span class="font-medium" :class="tone(hasComparable ? changePercent(totals.before_cost, totals.after_cost) : null)">{{ hasComparable ? percent(totals.before_cost, totals.after_cost) : '—' }}</span> · {{ t('admin.usageShaping.percent') }}</p>
        </div>
        <div class="card p-4 sm:col-span-2 xl:col-span-1">
          <p class="text-xs text-gray-500">{{ t('admin.usageShaping.tokenComparison') }}</p>
          <p class="mt-3 text-xl font-semibold tabular-nums">{{ hasComparable && !loading ? `${compact(totals.before_tokens)} → ${compact(totals.after_tokens)}` : '—' }}</p>
          <p class="mt-2 text-xs font-medium" :class="tone(hasComparable ? totals.after_tokens - totals.before_tokens : null)">{{ hasComparable ? `${signedTokens(totals.after_tokens - totals.before_tokens)} · ${percent(totals.before_tokens, totals.after_tokens)}` : '—' }}</p>
        </div>
      </div>
      <div class="grid gap-4 xl:grid-cols-5">
        <section class="card p-4 xl:col-span-3">
          <div class="mb-4 flex flex-wrap items-center justify-between gap-3">
            <h2 class="text-sm font-semibold">{{ t('admin.usageShaping.trend') }}</h2>
            <div class="flex gap-1">
              <button v-for="value in metrics" :key="value" type="button" class="rounded-md px-3 py-1.5 text-xs" :aria-pressed="metric === value" :class="metric === value ? 'bg-primary-50 text-primary-600 dark:bg-primary-900/20' : 'text-gray-500'" @click="metric = value">{{ t(`admin.usageShaping.${value}`) }}</button>
            </div>
            <Select v-model="granularity" :options="granularityOptions" class="w-28" size="sm" :aria-label="t('admin.usageShaping.trend')" @change="load" />
          </div>
          <div v-if="loading" class="flex h-72 items-center justify-center"><LoadingSpinner /></div>
          <div v-else-if="totals.requests" class="h-72"><Bar v-if="metric === 'requests'" :data="trendRequestData" :options="requestOptions" /><Line v-else :data="trendData" :options="trendOptions" /></div>
          <p v-else class="flex h-72 items-center justify-center text-sm text-gray-400">{{ t('admin.usageShaping.empty') }}</p>
        </section>
        <section class="card p-4 xl:col-span-2">
          <div class="mb-2 flex flex-wrap items-center justify-between gap-3"><h2 class="text-sm font-semibold">{{ t(`admin.usageShaping.${dimension}`) }} · Top 8</h2>
            <Select v-model="rankMetric" :options="rankMetricOptions" class="w-40" size="sm" :aria-label="t('admin.usageShaping.ranking')" />
          </div>
          <p class="mb-3 text-xs text-gray-500">{{ t('admin.usageShaping.topHint') }}</p>
          <div v-if="loading" class="flex h-72 items-center justify-center"><LoadingSpinner /></div>
          <div v-else-if="rankRows.length" class="h-72"><Bar :data="rankData" :options="rankOptions" /></div>
          <p v-else class="flex h-72 items-center justify-center text-sm text-gray-400">{{ t('admin.usageShaping.noComparable') }}</p>
        </section>
      </div>
      <section class="card overflow-hidden">
        <div class="flex flex-wrap items-center justify-between gap-3 p-4"><h2 class="text-sm font-semibold">{{ t(`admin.usageShaping.${dimension}`) }} · {{ t('admin.usageShaping.breakdown') }}</h2><div class="flex gap-2"><Select v-model="sort" :options="sortOptions" class="w-44" size="sm" :aria-label="t('admin.usageShaping.sort')" /><button type="button" class="btn btn-secondary" :disabled="!filteredRows.length" @click="exportAggregates">{{ t('admin.usageShaping.export') }}</button></div></div>
        <div class="overflow-x-auto"><table class="w-full whitespace-nowrap text-right text-sm tabular-nums"><thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-800"><tr><th class="px-4 py-3 text-left">{{ t(`admin.usageShaping.${dimension}`) }}</th><th class="px-4 py-3">{{ t('admin.usageShaping.requests') }}</th><th class="px-4 py-3">{{ t('admin.usageShaping.comparable') }}</th><th class="px-4 py-3">{{ t('admin.usageShaping.tokenComparison') }}</th><th class="px-4 py-3">{{ t('admin.usageShaping.beforeCost') }}</th><th class="px-4 py-3">{{ t('admin.usageShaping.afterCost') }}</th><th class="px-4 py-3">{{ t('admin.usageShaping.costDelta') }}</th><th class="px-4 py-3">{{ t('admin.usageShaping.percent') }}</th></tr></thead>
          <tbody class="divide-y divide-gray-100 dark:divide-dark-700"><tr v-for="row in paginatedRows" :key="entityKey(row.id)"><td class="max-w-72 truncate px-4 py-4 text-left" :title="entityName(row)">{{ entityName(row) }} <span v-if="row.id != null" class="text-xs text-gray-400">#{{ row.id }}</span></td><td class="px-4 py-4">{{ row.requests.toLocaleString() }}</td><td class="px-4 py-4">{{ row.comparable_requests.toLocaleString() }}</td><td class="px-4 py-4"><template v-if="row.comparable_requests"><div>{{ compact(row.before_tokens) }} → {{ compact(row.after_tokens) }}</div><div class="mt-1 text-xs" :class="tone(row.after_tokens - row.before_tokens)">{{ signedTokens(row.after_tokens - row.before_tokens) }} · {{ percent(row.before_tokens, row.after_tokens) }}</div></template><span v-else>—</span></td><td class="px-4 py-4" :title="row.before_cost.toFixed(8)">{{ row.comparable_requests ? money(row.before_cost) : '—' }}</td><td class="px-4 py-4" :title="row.after_cost.toFixed(8)">{{ row.comparable_requests ? money(row.after_cost) : '—' }}</td><td class="px-4 py-4" :class="tone(row.after_cost - row.before_cost)">{{ row.comparable_requests ? signedMoney(row.after_cost - row.before_cost) : '—' }}</td><td class="px-4 py-4 font-medium" :class="tone(row.comparable_requests ? changePercent(row.before_cost, row.after_cost) : null)">{{ row.comparable_requests ? percent(row.before_cost, row.after_cost) : '—' }}</td></tr><tr v-if="!paginatedRows.length"><td colspan="8" class="p-10 text-center text-gray-400">{{ loading ? t('common.loading') : t('admin.usageShaping.empty') }}</td></tr></tbody>
          <tfoot v-if="filteredRows.length" class="border-t border-gray-200 bg-gray-50 font-medium dark:border-dark-700 dark:bg-dark-800"><tr><td class="px-4 py-4 text-left">{{ t('admin.usageShaping.summary') }}</td><td class="px-4 py-4">{{ totals.requests.toLocaleString() }}</td><td class="px-4 py-4">{{ totals.comparable_requests.toLocaleString() }}</td><td class="px-4 py-4">{{ hasComparable ? `${compact(totals.before_tokens)} → ${compact(totals.after_tokens)}` : '—' }}<div v-if="hasComparable" class="mt-1 text-xs" :class="tone(totals.after_tokens - totals.before_tokens)">{{ signedTokens(totals.after_tokens - totals.before_tokens) }} · {{ percent(totals.before_tokens, totals.after_tokens) }}</div></td><td class="px-4 py-4">{{ hasComparable ? money(totals.before_cost) : '—' }}</td><td class="px-4 py-4">{{ hasComparable ? money(totals.after_cost) : '—' }}</td><td class="px-4 py-4" :class="tone(totals.after_cost - totals.before_cost)">{{ hasComparable ? signedMoney(totals.after_cost - totals.before_cost) : '—' }}</td><td class="px-4 py-4" :class="tone(hasComparable ? changePercent(totals.before_cost, totals.after_cost) : null)">{{ hasComparable ? percent(totals.before_cost, totals.after_cost) : '—' }}</td></tr></tfoot>
        </table></div>
        <Pagination v-if="filteredRows.length" :page="page" :page-size="pageSize" :total="filteredRows.length" @update:page="page = $event" @update:page-size="pageSize = $event; page = 1" />
      </section>
      <p class="text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.usageShaping.coverage') }} {{ t('admin.usageShaping.pricingNote') }} {{ t('admin.usageShaping.allFiltered') }}</p>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { Chart as ChartJS, CategoryScale, LinearScale, PointElement, LineElement, BarElement, Tooltip, Legend, type ChartOptions, type ChartEvent, type ActiveElement, type TooltipItem } from 'chart.js'
import { Line, Bar } from 'vue-chartjs'
import { useDark } from '@vueuse/core'
import AppLayout from '@/components/layout/AppLayout.vue'
import DateRangePicker from '@/components/common/DateRangePicker.vue'
import Pagination from '@/components/common/Pagination.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import { getShapingStats, type UsageShapingStat, type UsageShapingStats, type UsageShapingPoint } from '@/api/admin/usage'
import { sumMetrics, emptyMetrics, changePercent, entityKey } from '@/features/usage-shaping/metrics'

ChartJS.register(CategoryScale, LinearScale, PointElement, LineElement, BarElement, Tooltip, Legend)
const { t, locale } = useI18n()
const isDark = useDark()
const dimensions = ['account', 'group'] as const
const metrics = ['cost', 'tokens', 'requests'] as const
const dimension = ref<'account' | 'group'>('account')
const metric = ref<typeof metrics[number]>('cost')
const rankMetric = ref<'delta' | 'cost' | 'tokens' | 'requests'>('delta')
const granularity = ref<'auto' | 'hour' | 'day'>('auto')
const data = ref<UsageShapingStats | null>(null)
const selected = ref(''), search = ref(''), sort = ref('after')
const entityOptions = computed<SelectOption[]>(() => [
  { value: '', label: t('admin.usageShaping.all') },
  ...(data.value?.breakdown ?? []).map(row => ({ value: entityKey(row.id), label: entityName(row) })),
])
const granularityOptions = computed<SelectOption[]>(() => [
  { value: 'auto', label: t('admin.usageShaping.auto') },
  { value: 'day', label: t('admin.usageShaping.day') },
  { value: 'hour', label: t('admin.usageShaping.hour') },
])
const rankMetricOptions = computed<SelectOption[]>(() => [
  { value: 'delta', label: t('admin.usageShaping.costDelta') },
  { value: 'cost', label: t('admin.usageShaping.compareCost') },
  { value: 'tokens', label: t('admin.usageShaping.compareTokens') },
  { value: 'requests', label: t('admin.usageShaping.requests') },
])
const sortOptions = computed<SelectOption[]>(() => [
  { value: 'after', label: t('admin.usageShaping.sortAfter') },
  { value: 'delta', label: t('admin.usageShaping.sortDelta') },
  { value: 'tokens', label: t('admin.usageShaping.sortTokens') },
  { value: 'requests', label: t('admin.usageShaping.requests') },
])
const page = ref(1), pageSize = ref(20)
const dateString = (d: Date) => `${d.getFullYear()}-${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')}`
const today = new Date(), week = new Date(today)
week.setDate(week.getDate()-6)
const startDate = ref(dateString(week)), endDate = ref(dateString(today))
const loading = ref(false), error = ref('')
let controller: AbortController | undefined
const entityName = (row: UsageShapingStat) => row.name || t(row.id == null ? 'admin.usageShaping.ungrouped' : 'admin.usageShaping.deleted')
const filteredRows = computed(() => {
  const query = search.value.trim().toLowerCase()
  return (data.value?.breakdown ?? []).filter(row => (!selected.value || entityKey(row.id) === selected.value) && (!query || entityName(row).toLowerCase().includes(query) || String(row.id ?? '').includes(query)))
})
const sortedRows = computed(() => [...filteredRows.value].sort((a,b) => sort.value === 'delta' ? Math.abs(b.after_cost-b.before_cost)-Math.abs(a.after_cost-a.before_cost) : sort.value === 'tokens' ? Math.abs(b.after_tokens-b.before_tokens)-Math.abs(a.after_tokens-a.before_tokens) : sort.value === 'requests' ? b.requests-a.requests : b.after_cost-a.after_cost))
const paginatedRows = computed(() => sortedRows.value.slice((page.value-1)*pageSize.value,page.value*pageSize.value))
const totals = computed(() => sumMetrics(filteredRows.value))
const hasComparable = computed(() => totals.value.comparable_requests > 0)
const coverage = computed(() => totals.value.requests ? (totals.value.comparable_requests/totals.value.requests*100).toFixed(1) : '0.0')
const money = (n: number) => '$'+n.toLocaleString('en-US',{minimumFractionDigits:4,maximumFractionDigits:4})
const compact = (n: number) => n.toLocaleString(locale.value,{notation:'compact',maximumFractionDigits:2})
const signedMoney = (n: number) => `${n > 0 ? '+' : n < 0 ? '−' : ''}${money(Math.abs(n))}`
const signedTokens = (n: number) => `${n > 0 ? '+' : n < 0 ? '−' : ''}${compact(Math.abs(n))}`
const percent = (before: number, after: number) => { const value = changePercent(before,after); return value == null ? '—' : `${value>0?'+':''}${value.toFixed(1)}%` }
const direction = (n: number | null) => n != null && n > 1e-10 ? 'positive' : n != null && n < -1e-10 ? 'negative' : 'neutral'
const tone = (n: number | null) => ({positive:'text-emerald-600 dark:text-emerald-400',negative:'text-rose-600 dark:text-rose-400',neutral:'text-gray-500'})[direction(n)]
const points = computed(() => {
  const ids = new Set(filteredRows.value.map(r=>entityKey(r.id)))
  const buckets = new Map<string, UsageShapingPoint>()
  for (const bucket of data.value?.buckets ?? []) buckets.set(new Date(bucket).toISOString(),{...emptyMetrics(),id:null,bucket})
  for (const point of data.value?.series ?? []) {
    if (!ids.has(entityKey(point.id))) continue
    const key = new Date(point.bucket).toISOString()
    const target = buckets.get(key) ?? {...emptyMetrics(),id:null,bucket:point.bucket}
    Object.assign(target,sumMetrics([target,point])); buckets.set(key,target)
  }
  return [...buckets.values()].sort((a,b)=>Date.parse(a.bucket)-Date.parse(b.bucket))
})
const colors = computed(()=>({text:isDark.value?'#cbd5e1':'#64748b',grid:isDark.value?'#293548':'#edf0f5',before:'#94a3b8',after:isDark.value?'#a5b4fc':'#6366f1',positive:isDark.value?'#34d399':'#059669',negative:isDark.value?'#fb7185':'#e11d48',neutral:isDark.value?'#9ca3af':'#6b7280'}))
const labels = computed(()=>points.value.map(p=>new Date(p.bucket).toLocaleString(locale.value,{month:'2-digit',day:'2-digit',...(data.value?.granularity==='hour'?{hour:'2-digit',minute:'2-digit',hour12:false}:{} )})))
const trendData = computed(()=>({labels:labels.value,datasets:(['before','after'] as const).map(stage=>({label:t(`admin.usageShaping.${stage}`),data:points.value.map(p=>p.requests && !p.comparable_requests ? null : metric.value==='cost'?p[`${stage}_cost`]:p[`${stage}_tokens`]),borderColor:colors.value[stage],backgroundColor:colors.value[stage],borderDash:stage==='before'?[5,4]:[],borderWidth:2,pointRadius:points.value.length>40?0:2,tension:.2,spanGaps:false}))}))
const trendRequestData = computed(()=>({labels:labels.value,datasets:[{label:t('admin.usageShaping.requests'),data:points.value.map(p=>p.requests),backgroundColor:colors.value.after,borderRadius:3}]}))
function chartBase(horizontal = false) {
  return {responsive:true,maintainAspectRatio:false,animation:false as const,interaction:{intersect:false,mode:'index' as const},plugins:{legend:{labels:{color:colors.value.text,usePointStyle:true,font:{size:11}}}},scales:{x:{grid:{display:!horizontal,color:colors.value.grid},ticks:{color:colors.value.text,maxTicksLimit:7}},y:{beginAtZero:true,grid:{color:colors.value.grid},ticks:{color:colors.value.text}}}}
}
const trendOptions = computed<ChartOptions<'line'>>(()=>({ ...chartBase(),plugins:{...chartBase().plugins,tooltip:{footerColor:ctx=>{const p=points.value[ctx.tooltip.dataPoints[0]?.dataIndex];return colors.value[direction(p?.comparable_requests ? metric.value==='cost'?p.after_cost-p.before_cost:p.after_tokens-p.before_tokens : null)]},callbacks:{label:(ctx:TooltipItem<'line'>)=>`${ctx.dataset.label}: ${ctx.raw == null?'—':metric.value==='cost'?money(Number(ctx.raw)):Number(ctx.raw).toLocaleString()}`,footer:(items:TooltipItem<'line'>[])=>{
  const p=points.value[items[0]?.dataIndex];if(!p)return '';if(!p.comparable_requests)return t('admin.usageShaping.noComparable')
  const b=metric.value==='cost'?p.before_cost:p.before_tokens,a=metric.value==='cost'?p.after_cost:p.after_tokens
  return `${t('admin.usageShaping.delta')}: ${metric.value==='cost'?signedMoney(a-b):signedTokens(a-b)} (${percent(b,a)})\n${t('admin.usageShaping.comparable')}: ${p.comparable_requests.toLocaleString()}`
}}}},scales:{...chartBase().scales,y:{...chartBase().scales.y,ticks:{color:colors.value.text,callback:value=>metric.value==='cost'?'$'+Number(value).toLocaleString():compact(Number(value))}}}}))
const requestOptions = computed<ChartOptions<'bar'>>(()=>({...chartBase(),scales:{...chartBase().scales,y:{...chartBase().scales.y,ticks:{color:colors.value.text,precision:0}}},plugins:{...chartBase().plugins,tooltip:{callbacks:{footer:(items:TooltipItem<'bar'>[])=>{const p=points.value[items[0]?.dataIndex];return p?`${t('admin.usageShaping.comparable')}: ${p.comparable_requests.toLocaleString()} / ${p.requests.toLocaleString()}`:''}}}}}))
const rankRows = computed(()=>filteredRows.value.filter(r=>rankMetric.value==='requests'||r.comparable_requests>0).slice().sort((a,b)=>rankMetric.value==='delta'?Math.abs(b.after_cost-b.before_cost)-Math.abs(a.after_cost-a.before_cost):rankMetric.value==='requests'?b.requests-a.requests:rankMetric.value==='tokens'?b.after_tokens-a.after_tokens:b.after_cost-a.after_cost).slice(0,8))
const rankData = computed(()=>({labels:rankRows.value.map(entityName),datasets:rankMetric.value==='delta'?[{label:t('admin.usageShaping.costDelta'),data:rankRows.value.map(r=>r.after_cost-r.before_cost),backgroundColor:rankRows.value.map(r=>colors.value[direction(r.after_cost-r.before_cost)]),borderRadius:3}]:rankMetric.value==='requests'?[{label:t('admin.usageShaping.requests'),data:rankRows.value.map(r=>r.requests),backgroundColor:colors.value.after,borderRadius:3}]:(['before','after'] as const).map(stage=>({label:t(`admin.usageShaping.${stage}`),data:rankRows.value.map(r=>rankMetric.value==='cost'?r[`${stage}_cost`]:r[`${stage}_tokens`]),backgroundColor:colors.value[stage],borderRadius:3}))}))
const rankOptions = computed<ChartOptions<'bar'>>(()=>({...chartBase(true),indexAxis:'y',onClick:(_event:ChartEvent,elements:ActiveElement[])=>{const row=rankRows.value[elements[0]?.index];if(row)selected.value=entityKey(row.id)},scales:{x:{...chartBase(true).scales.x,beginAtZero:true,ticks:{color:colors.value.text,callback:value=>rankMetric.value==='delta'||rankMetric.value==='cost'?'$'+Number(value).toLocaleString():compact(Number(value))}},y:{...chartBase(true).scales.y,grid:{display:false},ticks:{color:colors.value.text,callback:(_value,index)=>{const n=rankRows.value[index]?entityName(rankRows.value[index]):'';return n.length>18?n.slice(0,18)+'…':n}}}},plugins:{...chartBase().plugins,tooltip:{callbacks:{label:(ctx:TooltipItem<'bar'>)=>`${ctx.dataset.label}: ${rankMetric.value==='delta'||rankMetric.value==='cost'?money(Number(ctx.raw)):Number(ctx.raw).toLocaleString()}`,footer:()=>t('admin.usageShaping.rankHint')}}}}))
async function load() {
  controller?.abort();const current=new AbortController();controller=current;error.value='';loading.value=true;data.value=null;page.value=1
  const days = (Date.parse(`${endDate.value}T00:00:00Z`) - Date.parse(`${startDate.value}T00:00:00Z`)) / 86400000 + 1
  if(!Number.isFinite(days)||days<1||days>366||(granularity.value==='hour'&&days>31)){error.value=t('admin.usageShaping.rangeError');loading.value=false;return}
  try {const result=await getShapingStats({dimension:dimension.value,start_date:startDate.value,end_date:endDate.value,timezone:Intl.DateTimeFormat().resolvedOptions().timeZone,...(granularity.value==='auto'?{}:{granularity:granularity.value})},current.signal);if(!current.signal.aborted)data.value=result}
  catch {if(!current.signal.aborted)error.value=t('admin.usageShaping.failed')}
  finally{if(controller===current)loading.value=false}
}
function exportAggregates() {
  const headers=[t(`admin.usageShaping.${dimension.value}`),t('admin.usageShaping.requests'),t('admin.usageShaping.comparable'),`${t('admin.usageShaping.before')} token`,`${t('admin.usageShaping.after')} token`,t('admin.usageShaping.beforeCost'),t('admin.usageShaping.afterCost'),t('admin.usageShaping.costDelta'),t('admin.usageShaping.percent')]
  const csv='\uFEFF'+[headers,...sortedRows.value.map(r=>[entityName(r),r.requests,r.comparable_requests,r.comparable_requests?r.before_tokens:'',r.comparable_requests?r.after_tokens:'',r.comparable_requests?r.before_cost.toFixed(10):'',r.comparable_requests?r.after_cost.toFixed(10):'',r.comparable_requests?(r.after_cost-r.before_cost).toFixed(10):'',r.comparable_requests?percent(r.before_cost,r.after_cost):''])].map(row=>row.map(v=>'"'+String(v).replace(/"/g,'""')+'"').join(',')).join('\r\n')
  const url=URL.createObjectURL(new Blob([csv],{type:'text/csv;charset=utf-8'}));const a=document.createElement('a');a.href=url;a.download=`usage-shaping-${dimension.value}-${startDate.value}-${endDate.value}.csv`;a.click();URL.revokeObjectURL(url)
}
watch(dimension,()=>{selected.value='';search.value='';void load()})
watch([selected,search,sort],()=>{page.value=1})
onMounted(load)
onUnmounted(()=>controller?.abort())
</script>
