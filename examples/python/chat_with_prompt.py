"""Call the multi-tenant gateway with the OpenAI Python SDK.

The gateway is OpenAI-compatible, so the stock SDK works: point base_url at
the gateway and use a tenant API key. Versioned prompts are an extension
field, passed with extra_body.

    pip install -r requirements.txt
    MTIG_API_KEY=sk-dev-acme-support python chat_with_prompt.py
"""

import os

import openai
from openai import OpenAI

client = OpenAI(
    base_url=os.environ.get("MTIG_BASE_URL", "http://localhost:8080/v1"),
    api_key=os.environ.get("MTIG_API_KEY", "sk-dev-acme-support"),
)

# "support-bot" is a tenant alias: the gateway routes it to the shared
# Llama-3.1-8B pool with the support-lora-v2 adapter.
MODEL = "support-bot"
PROMPT = {
    "id": "support-agent",
    "label": "production",  # or "version": 3 to pin one
    "variables": {"company": "Acme Cloud", "tier": "enterprise"},
}


def ask_once() -> None:
    # with_raw_response exposes the gateway's headers alongside the parsed body.
    raw = client.chat.completions.with_raw_response.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Our bucket returns 403 since this morning."}],
        max_tokens=64,
        extra_body={"prompt": PROMPT},
    )
    completion = raw.parse()
    print(f"prompt {raw.headers.get('x-prompt-id')} v{raw.headers.get('x-prompt-version')}, "
          f"{raw.headers.get('x-ratelimit-remaining-tokens')} tokens left this minute")
    print("reply:", completion.choices[0].message.content)
    print("usage:", completion.usage)


def ask_streaming() -> None:
    stream = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "Why is my invoice higher this month?"}],
        max_tokens=64,
        stream=True,
        stream_options={"include_usage": True},
        extra_body={"prompt": {**PROMPT, "label": "staging"}},
    )
    print("stream: ", end="")
    for chunk in stream:
        if chunk.choices and chunk.choices[0].delta.content:
            print(chunk.choices[0].delta.content, end="", flush=True)
        if chunk.usage:
            print(f"\nstream usage: {chunk.usage.total_tokens} tokens")


if __name__ == "__main__":
    try:
        ask_once()
        ask_streaming()
    except openai.RateLimitError as err:
        # Token budget exhausted: the gateway sets Retry-After.
        print("rate limited, retry after", err.response.headers.get("retry-after"), "s:", err.message)
