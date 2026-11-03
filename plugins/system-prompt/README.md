# System Prompts

A CLIProxyAPI plugin that appends your own text to the system prompt of
requests for the models you choose. It edits the request as the client sent it,
before CLIProxyAPI translates it for the upstream or applies Claude cloaking, so
the text reaches every provider the way the client's own system prompt does.

## Set up

1. Install `system-prompt` from Management Center → Plugin Store.
2. Open Management Center → System Prompts, choose **New rule**, pick models,
   write the prompt and save.

Saving writes the rules to `config.yaml`, where you can also edit them:

```yaml
plugins:
  configs:
    system-prompt:
      enabled: true
      rules:
        - models: ["claude-*", "gpt-5.5"]
          prompt: |
            Answer in British English.
```

## Which requests get a prompt

A model entry is a model ID, or a pattern in which `*` matches any text; case
does not matter. It matches either the model the client asked for or the model
CLIProxyAPI resolved it to through aliases and model pools, and a thinking
suffix such as `gpt-5.5(high)` is ignored. When several rules match,
their prompts are appended in list order. Count-tokens requests get the same
text, so their counts include it.

## Limit a rule to callers

On a shared gateway, optional `callers` selectors let you set prompts per client
API key or user with a separate key. Configure selectors in YAML. The management
page edits models and prompts and preserves existing `callers` selectors when
saving.

Each entry is a caller scope, the caller identity CLIProxyAPI exposes to plugins,
rather than an API key. This example uses a synthetic scope that you must replace:

```yaml
rules:
  - models: ["*"]
    prompt: "Follow the organization's writing guidelines."
  - models: ["gpt-*"]
    callers:
      ["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
    prompt: "Answer in Swedish."
```

Put these rules under `plugins.configs.system-prompt` as in the setup example.
Both the model and caller scope must match a restricted rule. Matching prompts
still follow config order.

| `callers` value                       | Behavior when the model matches                                   |
| ------------------------------------- | ----------------------------------------------------------------- |
| Absent                                | Apply to every caller, including requests without a caller scope. |
| Nonempty list of valid scopes         | Apply only to a listed caller scope.                              |
| Empty list, null, or a non-list value | Disable the rule and report a problem.                            |
| List with any malformed entry         | Disable the entire rule and report a problem.                     |

Entries must contain 64 hexadecimal characters after trimming whitespace and
converting to lowercase. A missing or non-string request scope matches only
rules without `callers`. The plugin reads `Metadata["caller_scope"]`, so it also
works with v8 hosts whose request type has no dedicated `CallerScope` field.
The plugin's status endpoint reports invalid rules in `problems`.

To compute a caller scope, set `KEY` privately to the client API key without
surrounding whitespace, then run:

```sh
printf 'cli-proxy-api:caller-scope:v1\0%s' "$KEY" | shasum -a 256
```

Copy only the 64-character digest into `callers`. Do not put the API key in the
plugin config or commit it. Users who share a key share a caller scope.

## Where the text goes

The prompt follows the client's system prompt after a blank line. When the
client sent none, the prompt becomes the system prompt.

| Client API              | Appended to                                                                    |
| ----------------------- | ------------------------------------------------------------------------------ |
| Claude Messages         | `system`: its last text block, or the string                                   |
| OpenAI Chat Completions | the last system or developer message that opens the conversation, or a new one |
| OpenAI Responses        | `instructions`                                                                 |
| Gemini                  | the last text part of `systemInstruction`                                      |

Requests without a conversation, such as embeddings and legacy completions,
pass unchanged.

## Claude cloaking

Cloaking sees the appended text as part of the client's system prompt. By
default it keeps it with the client's prompt; with the cloak setting
`strict-mode: true`, which drops caller system prompts, it drops it too.

The text extends the last system block instead of adding one, because
CLIProxyAPI recognizes Claude Code's helper requests by their number of system
blocks and would cloak them otherwise.

Payload rules (`payload.override` on `system`) differ: they run after cloaking
and replace the field, including the blocks cloaking adds.
