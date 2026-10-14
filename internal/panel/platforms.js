'use strict';
let pfSelected = 'loomy', pfState = null, pfGeneration = 0;
let pfAccounts = [], pfBalances = new Map(), pfBusy = false;
const pfLogins = new Map();
async function pfRPC(method, payload = {}) {
  const response = await api('platforms/rpc', {method:'POST',body:JSON.stringify({method,payload})});
  if (!response.ok) throw new Error(response.error?.message || '渠道操作失败');
  return response.value;
}
async function pfRun(action) {
  if (pfBusy) return;
  pfBusy = true; $('pfResult').textContent = '正在处理…';
  document.querySelectorAll('#pfActions button').forEach(el => el.disabled = true);
  try { await action(); } catch (e) { $('pfResult').textContent = e.message; }
  finally { pfBusy = false; document.querySelectorAll('#pfActions button').forEach(el => el.disabled = false); pfRenderActions(); }
}
function pfRenderActions() {
  const provider = pfState?.providers?.find(p => p.id === pfSelected);
  if (!provider) return;
  $('pfClaim').hidden = !provider.claim;
  $('pfLogin').hidden = provider.loginMode === 'import';
  $('pfAuto').disabled = !provider.auto || pfBusy;
  $('pfAuto').checked = provider.auto && provider.enabled !== false;
  $('pfLockWrap').hidden = !['buddy','workbuddy','loomy'].includes(pfSelected);
}
async function loadPlatforms() {
  try {
    pfState = await api('platforms');
    if (!pfState.available) {
      $('pfState').innerHTML = '<p>原生 Go 平台功能尚未启用。</p><button id="pfEnable">启用平台功能（重启后生效）</button>';
      $('pfEnable').onclick = () => pfRun(async () => { await api('config',{method:'POST',body:JSON.stringify({platforms:{enabled:true}})}); $('pfResult').textContent='已保存，请重启此网关进程后使用。'; });
      $('pfActions').hidden = true; return;
    }
    $('pfState').innerHTML = '<label><input type="checkbox" id="pfGlobalAuto"' + (pfState.enabled ? ' checked' : '') + '> 每日自动签到</label> <span class="hint">启动 30 秒后检查，按账号独立记录，跨天继续运行，失败 30 分钟后重试。</span>';
    $('pfGlobalAuto').onchange = e => pfRun(async () => { await api('platforms/schedule',{method:'POST',body:JSON.stringify({enabled:e.target.checked})}); await loadPlatforms(); });
    $('pfCards').innerHTML = pfState.providers.map(p => '<button class="' + (p.id===pfSelected?'primary':'') + '" data-provider="'+esc(p.id)+'">'+esc(p.name)+'</button>').join('');
    $('pfCards').querySelectorAll('button').forEach(button => button.onclick = () => { if (pfBusy) return; pfSelected=button.dataset.provider; pfBalances.clear(); $('pfLoginState').replaceChildren(); $('pfModelList').replaceChildren(); $('pfResult').textContent=''; loadPlatforms(); });
    $('pfActions').hidden=false; pfRenderActions();
    const p = pfState.providers.find(row=>row.id===pfSelected);
    $('pfResult').textContent = p.error || (p.lastDate ? '最近完成签到：'+p.lastDate : '尚无签到记录');
    await pfLoadAccounts();
  } catch(e) { $('pfState').textContent='读取失败：'+e.message; }
}
function pfRenderAccounts() {
  $('pfAccounts').innerHTML=pfAccounts.length ? pfAccounts.map(account=>{
    const row=pfBalances.get(account.id), balance=row?.balance;
    let amount='尚未查询';
    if (row) amount=row.error || (balance === null ? '未知' : typeof balance==='object' ? String(balance?.total ?? balance?.balance ?? balance?.remaining ?? '—') : String(balance ?? '—'));
    const unit={credit:' 积分',token:' Token',USD:' USD',quota:'%'}[pfState.providers.find(p=>p.id===pfSelected)?.unit] || '';
    if (row && !row.error && balance) amount += unit;
    if (pfSelected==='gemini' && balance?.packages) amount=balance.packages.map(p=>(p.name || '配额')+'：'+p.remaining+'%').join(' · ');
    if (row?.extra?.accountTier?.label) amount+=' · '+row.extra.accountTier.label;
    return '<tr><td>'+esc(account.nickname || account.id)+'</td><td>'+ (account.enabled===false?'已停用':'已启用') +'</td><td>'+esc(amount)+'</td><td><button data-action="toggle" data-id="'+esc(account.id)+'">'+(account.enabled===false?'启用':'停用')+'</button> <button data-action="refresh" data-id="'+esc(account.id)+'">续期</button> <button data-action="delete" data-id="'+esc(account.id)+'">删除</button></td></tr>';
  }).join('') : '<tr><td colspan="4">暂无账号，点击“添加账号”完成平台登录，或导入原项目的账号备份。</td></tr>';
  $('pfAccounts').querySelectorAll('button').forEach(button=>button.onclick=()=>pfRun(async()=>{
    const account=pfAccounts.find(a=>a.id===button.dataset.id), action=button.dataset.action;
    if (action==='delete' && !confirm('删除此账号及其保存的凭据？')) return;
    await pfRPC(action==='toggle'?'account.update':action==='delete'?'account.delete':'account.refresh',action==='toggle'?{accountId:account.id,patch:{enabled:account.enabled===false}}:{accountId:account.id});
    await pfLoadAccounts(); $('pfResult').textContent='已完成';
  }));
}
async function pfLoadAccounts() {
  const provider=pfSelected, generation=++pfGeneration;
  const data=await pfRPC('account.list',{provider});
  if (generation!==pfGeneration || provider!==pfSelected) return;
  pfAccounts=data.accounts;pfRenderAccounts();
  if (['buddy','workbuddy','loomy'].includes(provider)) {
    const lock=await pfRPC('credits.permanentLock',{provider});
    if (generation===pfGeneration && provider===pfSelected) $('pfLock').checked=lock.locked===true;
  }
}
$('pfReload').onclick=()=>{ if (!pfBusy) loadPlatforms(); };
$('pfBalance').onclick=()=>pfRun(async()=>{
  const result=await pfRPC('credits.balances',{provider:pfSelected,force:true});
  pfBalances=new Map(result.accounts.map(row=>[row.accountId,row])); pfRenderAccounts();
  const failed=result.accounts.filter(row=>row.error || row.balance===null).length;
  $('pfResult').textContent='已查询 '+result.accounts.length+' 个账号'+(failed?'，其中 '+failed+' 个未能查询，原因见账号列表。':'。');
});
$('pfClaim').onclick=()=>pfRun(async()=>{
  const result=await api('platforms/checkin',{method:'POST',body:JSON.stringify({provider:pfSelected})});
  if (!result.ok) throw new Error(result.error?.message || '领取失败');
  const summary=result.value.summary, gains=summary.totalByUnit || {credit:summary.totalCredit};
  const amounts=Object.entries(gains).filter(([,amount])=>amount>0).map(([unit,amount])=>'+'+Number(amount).toLocaleString()+(unit==='token'?' Token':' 积分')).join('，');
  const failures=(result.value.results || []).filter(row=>row.outcome?.kind==='failed'||row.outcome?.kind==='inactive').map(row=>(row.nickname||row.accountId)+'：'+(row.outcome.message||'当前不可领取'));
  $('pfResult').textContent='领取成功 '+summary.claimed+' 个，已领取 '+summary.alreadyClaimed+' 个，失败 '+summary.failed+' 个'+(amounts?'（'+amounts+'）':'')+(failures.length?'\n'+failures.join('\n'):'');
  pfBalances.clear();pfRenderAccounts();
});
$('pfAuto').onchange=e=>pfRun(async()=>{await api('platforms/schedule',{method:'POST',body:JSON.stringify({provider:pfSelected,enabled:e.target.checked})});await loadPlatforms();});
$('pfLock').onchange=e=>pfRun(async()=>{await pfRPC('credits.permanentLock',{provider:pfSelected,locked:e.target.checked});$('pfResult').textContent='积分保护设置已保存';});
$('pfLogin').onclick=()=>{
  if(pfBusy)return;
  const smsMode=pfState?.providers?.find(p=>p.id===pfSelected)?.loginMode==='sms';
  const popup=smsMode?null:window.open('about:blank','_blank');
  if(popup)popup.opener=null;
  pfRun(async()=>{
    const provider=pfSelected;
    try {
      const result=await pfRPC('account.create',{provider});
      if (result.loginMode==='sms') { pfSMSForm(result.accountId,provider); return; }
      const url=new URL(result.loginUrl,location.href);
      if (!['https:','http:'].includes(url.protocol)) throw new Error('登录地址协议不受支持');
      if(popup)popup.location.href=url.href;
      const link=document.createElement('a');link.href=url.href;link.target='_blank';link.rel='noopener noreferrer';link.textContent='打开平台登录页面 ↗';
      $('pfLoginState').replaceChildren(link);
      $('pfResult').textContent='请在打开的页面完成登录；等待最长 15 分钟。';
      const poll=async()=>{
        try {
          const state=await pfRPC('login.poll',{accountId:result.accountId});
          if(state.done){clearInterval(pfLogins.get(result.accountId));pfLogins.delete(result.accountId);$('pfLoginState').textContent=state.success?'登录完成':(state.error || '登录失败');if(provider===pfSelected)await pfLoadAccounts();}
        }catch(e){clearInterval(pfLogins.get(result.accountId));pfLogins.delete(result.accountId);$('pfLoginState').textContent=e.message;}
      };
      const timer=setInterval(poll,3000);pfLogins.set(result.accountId,timer);setTimeout(()=>{clearInterval(timer);pfLogins.delete(result.accountId);},15*60000);
      await pfLoadAccounts();
    }catch(e){popup?.close();throw e;}
  });
};
$('pfModels').onclick=()=>pfRun(async()=>{
  const result=await pfRPC('model.list',{provider:pfSelected});
  $('pfModelList').innerHTML=(result.models || []).map(model=>'<label style="display:block;margin:8px 0"><input type="checkbox" data-model="'+esc(model.id)+'"'+(model.disabled?'':' checked')+'> '+esc(model.name || model.id)+' <code>'+esc(pfSelected+'/'+model.id)+'</code></label>').join('') || '当前没有可用模型';
  $('pfModelList').querySelectorAll('input').forEach(input=>input.onchange=()=>pfRun(async()=>{await pfRPC('model.setDisabled',{provider:pfSelected,modelId:input.dataset.model,disabled:!input.checked});$('pfResult').textContent='模型设置已保存';}));
  $('pfResult').textContent='模型 ID 可用于本机 /v1/chat/completions、/v1/responses 和 /v1/messages。';
});
$('pfExport').onclick=()=>pfRun(async()=>{
  const result=await pfRPC('backup.export');const url=URL.createObjectURL(new Blob([JSON.stringify(result.payload,null,2)],{type:'application/json'}));
  const link=document.createElement('a');link.href=url;link.download='wb2api-platform-accounts.json';link.click();setTimeout(()=>URL.revokeObjectURL(url),60000);$('pfResult').textContent='备份已导出，包含账号凭据，请妥善保存。';
});
$('pfImport').onclick=()=>{if(!pfBusy)$('pfBackupFile').click();};
$('pfBackupFile').onchange=e=>pfRun(async()=>{
  const file=e.target.files[0];e.target.value='';if(!file)return;if(file.size>8*1024*1024)throw new Error('备份不能超过 8MB');
  if(!confirm('将合并此备份中的账号和凭据，继续导入？'))return;
  const result=await pfRPC('backup.import',{payload:JSON.parse(await file.text())});await loadPlatforms();$('pfResult').textContent='已导入 '+result.imported+' 个账号'+(result.skippedProviders?.length?'；当前未支持，已跳过：'+[...new Set(result.skippedProviders)].join('、'):'');
});
// app.js may finish its initial hash navigation before this separate script loads.
if (view === 'platforms') void loadPlatforms();
const pfOriginalAdd = $('btnAdd').onclick, pfOriginalRefresh = $('btnRefresh').onclick;
$('btnAdd').onclick = event => view === 'platforms' ? (pfState?.providers?.find(p=>p.id===pfSelected)?.loginMode==='import' ? $('pfCredential') : $('pfLogin')).click() : pfOriginalAdd?.(event);
$('btnRefresh').onclick = event => view === 'platforms' ? loadPlatforms() : pfOriginalRefresh?.(event);

function pfSMSForm(accountId,provider) {
  $('pfLoginState').innerHTML='<label>手机号 <input id="pfPhone" type="tel" autocomplete="tel" maxlength="11"></label> <button id="pfSendSMS">发送验证码</button> <label>验证码 <input id="pfSMSCode" autocomplete="one-time-code" maxlength="8"></label> <button id="pfSubmitSMS">登录</button>';
  $('pfResult').textContent='输入手机号，点击发送验证码后完成登录。';
  $('pfSendSMS').onclick=()=>pfRun(async()=>{
    const phone=$('pfPhone').value.trim();await pfRPC('login.sendSms',{provider,accountId,phone});$('pfResult').textContent='验证码已发送。';
  });
  $('pfSubmitSMS').onclick=()=>pfRun(async()=>{
    await pfRPC('login.submitSms',{provider,accountId,code:$('pfSMSCode').value.trim()});$('pfLoginState').replaceChildren();await pfLoadAccounts();$('pfResult').textContent='登录成功';
  });
}
$('pfCredential').onclick=()=>{
  if(pfBusy)return;
  $('pfLoginState').innerHTML='<label>账号昵称 <input id="pfCredentialName" autocomplete="off"></label><label style="display:block;margin-top:10px">登录凭据 JSON <textarea id="pfCredentialJSON" rows="5" style="display:block;width:100%" spellcheck="false" autocomplete="off" placeholder=\'{"access_token":"…"}\'></textarea></label><button id="pfCredentialSave">保存到本机</button>';
  $('pfCredentialSave').onclick=()=>pfRun(async()=>{
    const credential=JSON.parse($('pfCredentialJSON').value);await pfRPC('account.import',{provider:pfSelected,nickname:$('pfCredentialName').value.trim(),credential});$('pfLoginState').replaceChildren();await pfLoadAccounts();$('pfResult').textContent='账号已导入，凭据只保存于本机。';
  });
};
