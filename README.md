# jsonrpc
jsonrpc over websocket for golang &amp; typescript

jsonrpc2.0可完成远程服务接口调用, 本代码对原有jsonrpc中的源码进行补充，以及源码修复：

· 1. 完善院serveResponse接口中的解析出现的bug;
· 2. 添加 serveNotify接口通知消息;
