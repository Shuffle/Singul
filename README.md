<h1 align="center">

[![Singul Logo](https://shuffler.io/images/logos/singul.svg)](https://singul.io)

Singul

</h1>
<h4 align="center">
Connect to anything with a Singul line of code. Now open source 
</h4>

## Why
APIs and AI Agents should be easier to use and build. Singul solves both by being deterministic and reliable.

**Deterministic because:**
- LLMs can be unpredictable and unreliable
- Singul stores translations after the first use, and we have a global library for known translations
- You have full control of all translations
- It has a source of truth for APIs, and is not guessing

**Reliable Translations:**
- For your input, we store the format and know how to translate it after the first request
- For the output, we store the format and know how to translate it after the first request

## Usage
CLI
```
singul --help
singul jira list_tickets 
singul outlook send_mail --subject="hoy"
```

Code (python)
```
import singul
singul.send_mail("gmail", subject="hoy")
tickets = singul.list_tickets("jira")
```

API
```
curl https://singul.io/api/send_mail -d '{"app": "gmail", "subject": "hoy"}'
curl https://singul.io/api/list_tickets -d '{"app": "jira"}'
```

## Distributed usage
Available in the following ways:
- Local CLI
-  

## Storage
The following data type is stored with Singul during usage
- Authentication 
- Translation files

## How we use it at Shuffle
- In workflows 
