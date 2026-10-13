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
