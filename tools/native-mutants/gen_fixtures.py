import json, os
out='src/internal/provider/anthropic/testdata'
prov="synthetic-from-docs: platform.claude.com/docs/en/build-with-claude/streaming and api/errors, read 2026-11-06; replace with real captures when a Console API key exists"
def sse(evs): return ''.join(f"event: {n}\ndata: {json.dumps(d,separators=(',',':'))}\n\n" for n,d in evs)
def ms(i=25,o=1): return ("message_start",{"type":"message_start","message":{"id":"msg_syn","type":"message","role":"assistant","content":[],"model":"fixture-model","stop_reason":None,"stop_sequence":None,"usage":{"input_tokens":i,"output_tokens":o}}})
def cbs(i,b): return ("content_block_start",{"type":"content_block_start","index":i,"content_block":b})
def cbd(i,d): return ("content_block_delta",{"type":"content_block_delta","index":i,"delta":d})
def cbx(i): return ("content_block_stop",{"type":"content_block_stop","index":i})
def md(r,o): return ("message_delta",{"type":"message_delta","delta":{"stop_reason":r,"stop_sequence":None},"usage":{"output_tokens":o}})
stop=("message_stop",{"type":"message_stop"})
ping=("ping",{"type":"ping"})
fx={}
fx['text']=(200,{},sse([ms(),cbs(0,{"type":"text","text":""}),ping,cbd(0,{"type":"text_delta","text":"Hello"}),cbd(0,{"type":"text_delta","text":"!"}),cbx(0),md("end_turn",15),stop]),
  {"outcome":"ok","blocks":["text"],"stop_reason":"end_turn","input":25,"output":15})
fx['tool_use']=(200,{},sse([ms(472,2),cbs(0,{"type":"text","text":""}),cbd(0,{"type":"text_delta","text":"Checking."}),cbx(0),
  cbs(1,{"type":"tool_use","id":"toolu_syn1","name":"get_weather","input":{}}),cbd(1,{"type":"input_json_delta","partial_json":""}),
  cbd(1,{"type":"input_json_delta","partial_json":"{\"location\":"}),cbd(1,{"type":"input_json_delta","partial_json":" \"San Francisco, CA\"}"}),cbx(1),md("tool_use",89),stop]),
  {"outcome":"ok","blocks":["text","tool_use"],"stop_reason":"tool_use","input":472,"output":89,"tool":"get_weather"})
fx['thinking_signature']=(200,{},sse([ms(),cbs(0,{"type":"thinking","thinking":"","signature":""}),cbd(0,{"type":"thinking_delta","thinking":"Let me solve this step by step: 27 * 453"}),
  cbd(0,{"type":"thinking_delta","thinking":"\n12231"}),cbd(0,{"type":"signature_delta","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"}),cbx(0),
  cbs(1,{"type":"redacted_thinking","data":"EmwKAhgBEgy3va3pzix/LafPsn4a"}),cbx(1),
  cbs(2,{"type":"text","text":""}),cbd(2,{"type":"text_delta","text":"12231"}),cbx(2),md("end_turn",40),stop]),
  {"outcome":"ok","blocks":["thinking","redacted_thinking","text"],"stop_reason":"end_turn","input":25,"output":40,"signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"})
fx['max_tokens']=(200,{},sse([ms(),cbs(0,{"type":"text","text":""}),cbd(0,{"type":"text_delta","text":"cut"}),cbx(0),md("max_tokens",5),stop]),
  {"outcome":"ok","blocks":["text"],"stop_reason":"max_tokens","input":25,"output":5})
fx['unknown_stop']=(200,{},sse([ms(),cbs(0,{"type":"text","text":""}),cbd(0,{"type":"text_delta","text":"x"}),cbx(0),md("future_reason_2027",5),stop]),
  {"outcome":"ok","blocks":["text"],"stop_reason":"future_reason_2027","input":25,"output":5})
fx['midstream_error']=(200,{},sse([ms(),cbs(0,{"type":"text","text":""}),cbd(0,{"type":"text_delta","text":"Hel"}),("error",{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}})]),
  {"outcome":"stream_error","error_type":"overloaded_error"})
fx['http_529']=(529,{},json.dumps({"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_syn529"}),{"outcome":"api_error","status":529,"retry":True})
fx['http_429_retry_after']=(429,{"Retry-After":"12"},json.dumps({"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"},"request_id":"req_syn429"}),{"outcome":"api_error","status":429,"retry":True,"retry_after_s":12})
fx['http_429_spend_cap']=(429,{},json.dumps({"type":"error","error":{"type":"rate_limit_error","message":"You have reached your specified API usage limits"},"request_id":"req_synCap"}),{"outcome":"park"})
fx['http_400_spend_limit']=(400,{},json.dumps({"type":"error","error":{"type":"invalid_request_error","message":"Your workspace has reached its spend limit"},"request_id":"req_synSL"}),{"outcome":"park"})
fx['http_401']=(401,{},json.dumps({"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"},"request_id":"req_syn401"}),{"outcome":"api_error","status":401,"retry":False})
for name,(st,h,body,exp) in fx.items():
    json.dump({"scenario":name,"provenance":prov,"anthropic_version":"2026-10-21","status":st,"headers":dict(h,**{"Content-Type":"text/event-stream" if st==200 else "application/json"}),"request":{"model":"fixture-model","max_tokens":64,"stream":True,"messages":[{"role":"user","content":[{"type":"text","text":"fixture"}]}]},"response":body},open(f"{out}/{name}.json","w"),indent=1)
    json.dump(exp,open(f"{out}/{name}.expect.json","w"),indent=1)
print(len(fx),"fixtures")
