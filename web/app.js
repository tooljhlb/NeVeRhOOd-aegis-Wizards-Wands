'use strict';
const $=id=>document.getElementById(id);
let token=sessionStorage.getItem('ww_token')||'', session=null, models=[], running=false, abort=null, assistant=null, reasoning=null, attachment=null;
function setStatus(value){$('busy').textContent=value}
function modal(open){$('connect').classList.toggle('hidden',!open);if(open){$('token').value=token;$('token').focus()}}
async function api(path,options={}){
 const headers={'Authorization':`Bearer ${token}`,...(options.headers||{})};
 if(options.body && !(options.body instanceof FormData))headers['Content-Type']='application/json';
 const res=await fetch(path,{...options,headers});if(!res.ok){let error;try{error=(await res.json()).error}catch{error=`HTTP ${res.status}`};throw Error(error)}
 return res.json();
}
function node(tag,cls,text){const el=document.createElement(tag);if(cls)el.className=cls;if(text!==undefined)el.textContent=text;return el}
function addMessage(role,text='',thought=''){
 const wrap=node('div','message '+role);wrap.append(node('div','role',role==='user'?'YOU':'AI / '+$('model').value.toUpperCase()));
 const content=node('div','content',text);wrap.append(content);
 const reason=node('details','thought');reason.append(node('summary','', 'Model-provided reasoning'));reason.append(node('div','text',thought));if(thought)wrap.append(reason);
 $('messages').append(wrap);$('messages').scrollTop=$('messages').scrollHeight;
 return {content,reason,wrap};
}
function feed(type,label,detail=''){
 const wrap=node('div','feed-item'+(type.includes('error')||type.includes('failed')?' error':type.includes('approval')?' warn':type.includes('tool')?' mcp':''));wrap.append(node('span','bullet'));
 const body=node('div');body.append(node('b','',label),node('small','',detail.slice(0,260)));wrap.append(body);
 $('feed').prepend(wrap);while($('feed').children.length>80)$('feed').lastChild.remove();
}
function eventHandler(e){
 const data=e.data||{};
 switch(e.type){
  case 'turn.started':feed(e.type,'Turn started',`${data.model} · ${data.mode}`);break;
  case 'content.delta':if(!assistant)assistant=addMessage('assistant');assistant.content.textContent+=(data.text||'');$('messages').scrollTop=$('messages').scrollHeight;break;
  case 'reasoning.delta':if(!assistant)assistant=addMessage('assistant');if(!reasoning){reasoning=node('details','thought');reasoning.append(node('summary','','Model-provided reasoning'),node('div','reasoning-text',''));assistant.wrap.append(reasoning)}reasoning.querySelector('.reasoning-text').textContent+=(data.text||'');break;
  case 'tool.started':feed(e.type,`Tool ${data.tool}`,`Executing ${data.call_id||''}`);break;
  case 'tool.completed':feed(e.type,`Tool completed: ${data.tool}`,String(data.output||'').slice(0,260));break;
  case 'tool.failed':feed(e.type,`Tool failed: ${data.tool}`,data.error||'');break;
  case 'tool.denied':feed(e.type,`Tool denied: ${data.tool}`);break;
  case 'approval.required':$('approval').classList.remove('hidden');$('approvalText').textContent=`${data.tool}\n${data.arguments||''}`;feed(e.type,'Approval required',data.tool);break;
  case 'turn.completed':if(!assistant)assistant=addMessage('assistant',data.text||'');feed(e.type,'Turn completed',data.model||'');break;
  case 'turn.error':feed(e.type,'Execution error',data.error||'');if(!assistant)assistant=addMessage('assistant',`Error: ${data.error||'Unknown failure'}`);break;
  case 'turn.cancelled':feed(e.type,'Turn cancelled');break;
  case 'model.started':feed(e.type,'Model invoked',`${data.model} · step ${data.step}`);break;
  case 'model.tools_requested':feed(e.type,'Tools requested',`${data.count} tool call(s)`);break;
 }
}
async function stream(path,body){
 if(running)return;running=true;abort=new AbortController();$('send').disabled=true;$('stop').classList.remove('hidden');setStatus('RUNNING');assistant=null;reasoning=null;
 try{
  const res=await fetch(path,{method:'POST',headers:{'Authorization':`Bearer ${token}`,'Content-Type':'application/json'},body:JSON.stringify(body),signal:abort.signal});
  if(!res.ok)throw Error(`HTTP ${res.status}`);const reader=res.body.getReader();const decoder=new TextDecoder();let pending='';
  while(true){const {value,done}=await reader.read();if(done)break;pending+=decoder.decode(value,{stream:true});let end;while((end=pending.indexOf('\n\n'))>=0){const packet=pending.slice(0,end).replace(/\r/g,'');pending=pending.slice(end+2);const data=packet.split('\n').filter(l=>l.startsWith('data:')).map(l=>l.slice(5).trim()).join('\n');if(!data)continue;try{eventHandler(JSON.parse(data))}catch(err){console.error('Malformed event',err)}}}
 }catch(e){if(e.name!=='AbortError'){feed('turn.error','Network error',String(e));addMessage('assistant',`Connection error: ${e.message}`)}else{feed('turn.cancelled','Stopped by user')}}
 finally{running=false;abort=null;$('send').disabled=false;$('stop').classList.add('hidden');setStatus('READY');await refreshSessions();}
}
async function createSession(){const v=await api('/api/sessions',{method:'POST',body:JSON.stringify({model:$('model').value||'demo',mode:$('mode').value})});session=v.id;$('messages').replaceChildren();$('approval').classList.add('hidden');await refreshSessions();return v}
async function refreshSessions(){const items=await api('/api/sessions');const panel=$('sessions');panel.replaceChildren();for(const v of items){const b=node('button','session'+(v.id===session?' active':''),v.name+(v.pending?' ⚠':''));b.addEventListener('click',()=>openSession(v.id));panel.append(b)}}
async function openSession(id){if(running)return;session=id;const s=await api('/api/sessions/'+encodeURIComponent(id));$('model').value=s.model;$('mode').value=s.mode;$('messages').replaceChildren();assistant=null;reasoning=null;
 for(const m of s.messages||[]){if(m.role==='user'||m.role==='assistant'){if(m.content)addMessage(m.role,m.content)} }
 $('approval').classList.toggle('hidden',!s.pending);if(s.pending){const t=s.pending.calls?.[s.pending.index];$('approvalText').textContent=t?`${t.function.name}\n${t.function.arguments}`:'Pending tool'}await refreshSessions()}
async function send(){const value=$('prompt').value.trim();if(!value||running)return;if(!session)await createSession();$('approval').classList.add('hidden');$('prompt').value='';addMessage('user',value+(attachment?`\n[attachment: ${attachment.name} / ${attachment.id}]`:''));const message=value+(attachment?`\nAttached artifact ID: ${attachment.id} (${attachment.media_type}, ${attachment.bytes} bytes). Use artifact_inspect if needed.`: '');attachment=null;$('artifactTag').classList.add('hidden');await stream(`/api/sessions/${session}/chat`,{message,model:$('model').value,mode:$('mode').value,thinking:$('thinking').value})}
async function decide(approve){$('approval').classList.add('hidden');await stream(`/api/sessions/${session}/approval`,{approve})}
async function init(){try{const [data,modelList]=await Promise.all([api('/api/status'),api('/api/models')]);models=modelList;$('model').replaceChildren();for(const m of models){const o=node('option','',`${m.name} · ${m.provider}`);o.value=m.name;$('model').append(o)}$('model').value=data.default_model;$('dataState').textContent=data.data_adapter_configured?'Configured':'Optional / not connected';const defs=await api('/api/tools');$('tools').replaceChildren();for(const d of defs){$('tools').append(node('div','tool',d.function.name))}await refreshSessions();feed('system','Harness connected',`${models.length} model profile(s) · ${defs.length} tools`);modal(false)}catch(e){modal(true);$('connectError').textContent='Enter the server token to load the workspace. '+e.message}}
$('new').onclick=()=>createSession().catch(e=>feed('error','Cannot start',e.message));$('send').onclick=()=>send().catch(e=>feed('error','Send failed',e.message));$('prompt').addEventListener('keydown',e=>{if(e.key==='Enter'&&!e.shiftKey){e.preventDefault();send().catch(console.error)}});$('stop').onclick=()=>abort?.abort();$('approve').onclick=()=>decide(true);$('reject').onclick=()=>decide(false);
$('settings').onclick=()=>modal(true);$('closeSettings').onclick=()=>modal(false);$('saveToken').onclick=()=>{token=$('token').value.trim();sessionStorage.setItem('ww_token',token);$('connectError').textContent='';init()};
for(const b of document.querySelectorAll('[data-prompt]')){b.addEventListener('click',()=>{$('prompt').value=b.dataset.prompt;$('prompt').focus()})}
$('file').addEventListener('change',async e=>{const file=e.target.files[0];if(!file)return;const form=new FormData();form.append('file',file);try{attachment=await api('/api/artifacts',{method:'POST',body:form});$('artifactTag').textContent=`✦ ${attachment.name} · ${attachment.bytes} bytes`; $('artifactTag').classList.remove('hidden');feed('artifact','Artifact stored',attachment.name)}catch(err){feed('error','Attachment failed',err.message)} });
if(token)init();else modal(true);
