<h1 align="center">

[![Singul Logo](https://shuffler.io/images/logos/singul.svg)](https://singul.io)

Singul

</h1>
<h4 align="center">
Connect to anything with a Singul line of code. Now open source 
</h4>

## Why
APIs and AI Agents should be easier to use

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
