<h1 align="center">

[![Singul Logo](https://shuffler.io/images/logos/singul.svg)](https://singul.io)

Singul

</h1>
<h4 align="center">
Connect to anything with a Singul line of code. Now open source!
</h4>

## Why
APIs and AI Agents should be easier to use and build. Singul solves both by being deterministic, controllable and reliable.

**Deterministic because:**
- LLMs can be unpredictable and unreliable
- Singul stores translations after the first use, and we have a global library for known translations
- You have full control of all translations
- It has a source of truth for APIs, and is not guessing

**Reliable Translations:**
- For your input AND output, we store the format and know how to translate it after successful requests. This is then reusable in subsequent requests

**Stable Connections & stored authentication:**
- Singul is based on how we built [Shuffle](https://shuffler.io) and how we connect to APIs. We use the knowledge of Shuffle, including apps, categories, tags, actions, authentication mechanisms, code and more. 

## Usage
CLI
```
singul --help
singul list_tickets jira 
singul send_mail outlook --subject="hoy" --data="hello world" --to="test@example.com"
```

Code (python): Local OR Remote
```
import singul
singul.send_mail("gmail", subject="hoy")
tickets = singul.list_tickets("jira")
```

API: singul.io API
```
curl https://singul.io/api/send_mail -d '{"app": "gmail", "subject": "hoy"}'
curl https://singul.io/api/list_tickets -d '{"app": "jira"}'
```

## Local Example
```
go run *.go create_ticket jira --project=SHUF --title="title2" --content="cool new body here 2"
```
