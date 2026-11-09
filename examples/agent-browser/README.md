# agent-browser setup

Merge [node-config.json](node-config.json) into the intended worker profile, preserving other settings. Replace the placeholder with the absolute native executable path. Installation of agent-browser and Chrome needs approval; Control does not bundle or install them automatically.

See [browser setup, remote viewing and cleanup](../../docs/browser.md). With dependencies configured:

```sh
control browser run worker --session work -- open https://example.com
control browser run worker -- dashboard start --port 4848
control browser view worker --port 4848
```

Open the printed loopback URL on the orchestrator machine. Pause automation during manual input. Ctrl+C closes only the forward. Close the browser and dashboard explicitly when finished:

```sh
control browser run worker --session work -- close
control browser run worker -- dashboard stop
```

Use a distinct worker port for every concurrent owner dashboard. Live frames and upstream activity are not redacted. Do not put credentials or verification codes in CLI inputs or chat.
