import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {pathToFileURL} from "node:url";

// Usage: node examples/pi-proxy-websocket-smoke.mjs /path/to/pi-ai/dist
// Two tiny live generations verify actual WebSocket reuse/deltas. No credentials are printed.
const aiDist = process.argv[2];
if (!aiDist) throw new Error("Pass the installed pi-ai dist directory");
const native = await import(pathToFileURL(path.join(aiDist,"api/openai-codex-responses.js")).href);
const providers=JSON.parse(fs.readFileSync(path.join(os.homedir(),".config/pi/agent/models.json"),"utf8")).providers;
const provider=providers.cliproxyapi;
const model={...provider.models.find(m=>m.id==="gpt-5.5"),api:provider.api,provider:"cliproxyapi",baseUrl:provider.baseUrl};
const apiKey=JSON.parse(fs.readFileSync(path.join(os.homedir(),".local/share/cliproxyapi/secrets.json"),"utf8")).client_key;
const sessionId="cliproxyapi-ws-smoke-"+Date.now();
const context={messages:[{role:"user",content:"Reply with only the word ok.",timestamp:Date.now()}]};
try {
 for(let i=0;i<2;i++){
  const stream=native.stream(model,context,{apiKey,sessionId,transport:"websocket-cached",reasoningEffort:"low",signal:AbortSignal.timeout(45000)});
  for await (const _ of stream) {}
  const answer=await stream.result();
  if(answer.stopReason==="error"||answer.stopReason==="aborted") throw new Error(answer.errorMessage||answer.stopReason);
  context.messages.push(answer,{role:"user",content:"Again, reply with only ok.",timestamp:Date.now()});
  console.log(JSON.stringify({request:i+1,stopReason:answer.stopReason,usage:answer.usage}));
 }
 const stats=native.getOpenAICodexWebSocketDebugStats(sessionId);
 console.log(JSON.stringify({websocket:stats}));
 if(!stats||stats.connectionsCreated!==1||stats.connectionsReused<1||stats.deltaRequests<1||stats.sseFallbacks!==0) throw new Error("Expected reused cached-context WebSocket without SSE fallback");
} finally { native.closeOpenAICodexWebSocketSessions(sessionId); }
