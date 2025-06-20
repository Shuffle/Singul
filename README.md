<h1 align="center">

[<img src="https://shuffler.io/images/logos/singul.svg" alt="Singul Logo" width="100"/>](https://singul.io)

Singul

</h1>
<h4 align="center">
Connect to anything with a Singul line of code. Now open source (in Beta)!
</h4>

## Usage
Before starting, **set the OPENAI_API_KEY environment variable**. Other LLM's work as well - check out [LLM Controls](#llm-controls) further down for more information.

```
export OPENAI_API_KEY=key
```

## Examples
**CLI**
```bash
singul --help

# List tickets in the OCSF format
singul list_tickets jira 
singul list_tickets service_now 

# Send mail in the same format
singul send_mail outlook --subject="hoy" --data="hello world" --to="test@example.com"
singul send_mail gmail --subject="hoy" --data="hello world" --to="test@example.com"
```

**Code (python)**:
Install the library
```bash
pip install singul
```

Use it locally
```python
import singul

# List tickets in the OCSF format
tickets = singul.run("jira", action="list_tickets", max_items=10)
tickets = singul.run("service_now", action="list_tickets", max_items=10)

# Send mail in the same format
send_mail_response = singul.run("outlook", action="send_mail", subject="hoy", data="hello world" to="test@example.com")
send_mail_response = singul.run("gmail", action="send_mail", subject="hoy", data="hello world" to="test@example.com")
```

**API**: singul.io API with curl
This requires an API key from [shuffler.io](https://shuffler.io/register), which is used for storing authentication. 

```bash
# List tickets in the OCSF format
curl https://singul.io/api/list_tickets -d '{"app": "jira"}'
curl https://singul.io/api/list_tickets -d '{"app": "service_now"}'

# Send mail in the same format
curl https://singul.io/api/send_mail -d '{"app": "gmail", "subject": "hoy", data="hello world", to="test@example.com"}'
curl https://singul.io/api/send_mail -d '{"app": "outlook", "subject": "hoy", data="hello world", to="test@example.com"}'
```

## Why Singul
APIs and AI Agents should be easier to use and build. Singul solves both by being easy to use, deterministic and controllable. This reduces the barrier to entry for multi-tool development and allows you to build your own AI agents with ease. 

**Deterministic because:**
- LLMs can be unpredictable and unreliable
- Singul stores translations after the first use, and we have a global library for known translations
- You have full control of all translations
- It has a source of truth for APIs, and is not "guessing"

**Reliable Translations:**
- For your input AND output, we store the format and know how to translate it after successful requests. This is then reusable in subsequent requests
- Singul ensures all input fields ARE in the request, or fails out. You can modify the relevant files to update the body you want to send if this occurs after up to 5 request failures.

**Stable Connections & stored authentication:**
- Singul is based on how we built [Shuffle](https://shuffler.io) and how we connect to APIs. We use the knowledge of Shuffle, including apps, categories, tags, actions, authentication mechanisms, code and more. 

## LLM Controls
Set these environment variables to control the behavior of Singul. The AI/LLM section is **FOR NOW** only supporting the OpenAI format, but we intend to add more in the future to make it work well standalone. Examples of how to change to a different providers with environment variables is: 

* Local Ollama: OPENAI_API_URL=http://localhost:11434/v1, AI_MODEL=llama2
* Google Gemini: OPENAI_API_URL=https://generativelanguage.googleapis.com/v1beta/openai, OPENAI_API_KEY=<key>, AI_MODEL=gemini-2.0-flash
* xAI Grok: OPENAI_API_URL=https://api.x.ai/v1, OPENAI_API_KEY=<key>, AI_MODEL=grok-3-mini-fast-beta
* Anthropic: OPENAI_API_URL=https://api.anthropic.com/v1, OPENAI_API_KEY=<key>, AI_MODEL=claude-3-7-sonnet-20250219
* Deepseek: OPENAI_API_URL=https://api.deepseek.com, OPENAI_API_KEY=<key>, AI_MODEL=deepseek-reasoner
* Mistral: OPENAI_API_URL=https://api.mistral.ai/v1, OPENAI_API_KEY=<key>, AI_MODEL=codestral-latest
* Amazon Bedrock: NOT COMATIBLE YET :((
...

**Other OpenAI compatible API's also work**

## Environment Variables
**REQUIRED:**
```
OPENAI_API_KEY=<key> 	# The API key for the OpenAI API, if you want to use a different provider. Default: None
```

**Singul controls**
```
DEBUG=true 				# Enables debug mode
FILE_LOCATION=./files 	# The location of the files. Default: ./files
SHUFFLE_ENCRYPTION_MODIFIER=randomstringhere # Required for local encryption of authentication.

# Where to download and control standards from 
GIT_DOWNLOAD_USER=shuffle
GIT_DOWNLOAD_REPO=standards

# To upload results to a Datastore, find details on https://shuffler.io/admin. This is disabled by default
SHUFFLE_ORG="ORG_ID"
SHUFFLE_AUTHORIZATION="API KEY"
SHUFFLE_BACKEND="SHUFFLE BACKEND"
```

**LLM controls:**
```
AI_MODEL=<model> 			# The model to use for the LLM. We recommend reasoning models. Default: o4-mini
OPENAI_API_URL=<url> 	# The URL of the OpenAI API, if you want to use a different provider. Default: https://api.openai.com/v1/chat/completions
OPENAI_API_ORG=<org> 	# The organization ID for the OpenAI API, if you want to use a different provider. Default: None
```

## Local Test Example
```
go run *.go create_ticket jira --project=SHUF --title="title2" --content="cool new body here 2"
go run *.go list_tickets jira --max_results=2
```


## How it works
![image](https://github.com/user-attachments/assets/4f06b240-2010-40f6-965b-95a9e381f97e)

![image](https://github.com/user-attachments/assets/fb1a0c83-d021-4308-9074-b5af822e2302)

![image](https://github.com/user-attachments/assets/fc659b7c-dbbc-4c36-8e53-fc23b59995e3)


