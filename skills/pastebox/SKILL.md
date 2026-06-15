---
name: pastebox
description: Use when the user wants to share text, logs, or command output using the ROKFOSS Pastebox service (paste.krfoss.org). Provides instructions for uploading, viewing, password-protecting, and deleting pastes via curl.
---

# Pastebox Usage Skill

## Overview
This skill provides instructions for interacting with the ROKFOSS Pastebox service, a lightweight, `curl`-based text and log sharing service.
Public Service URL: **https://paste.krfoss.org**

## Core Workflows

### 1. Uploading Text or Logs
To upload text or command output, pipe the data directly into `curl` using `--data-binary @-`.

**Basic Upload:**
```bash
echo "hello world" | curl -X POST --data-binary @- https://paste.krfoss.org/
```

**Upload a File:**
```bash
curl -X POST --data-binary @my_log.txt https://paste.krfoss.org/
```
*Note: The server validates content and rejects binary files. Only text and logs are accepted.*

### 2. Password-Protected Uploads
To secure a paste with a randomly generated password, include the `usepassword: true` header.

```bash
echo "secret info" | curl -H "usepassword: true" -X POST --data-binary @- https://paste.krfoss.org/
```
*The response will include the URL and the generated password. Save the password, as it cannot be recovered.*

**Choosing your own password:** upload to `/pw/<password>` and that exact string becomes the password (e.g. `/pw/12345` → password `12345`). The segment right after `/pw/` is always the password, so even `temp`/`week` work as passwords (`/pw/week/week` → password `week`); an optional trailing `temp` or `week` adds a retention policy.

```bash
# password "12345"
echo "secret info" | curl -X POST --data-binary @- https://paste.krfoss.org/pw/12345

# password "12345" + burn-after-reading / + one-week retention
echo "secret info" | curl -X POST --data-binary @- https://paste.krfoss.org/pw/12345/temp
echo "secret info" | curl -X POST --data-binary @- https://paste.krfoss.org/pw/12345/week
```
*Note: a password placed in the URL path may appear in proxy/access logs. To avoid this, pass it via the `paste-custom-password` header, which also combines freely with any policy route:*
```bash
echo "secret info" | curl -H "paste-custom-password: 12345" -X POST --data-binary @- https://paste.krfoss.org/week
```

### 3. Viewing Pastes
**Standard View (Browser or curl):**
```bash
curl https://paste.krfoss.org/<PASTE_ID>
```

**Force Raw Text (Bypass HTML wrapper):**
```bash
curl "https://paste.krfoss.org/<PASTE_ID>?raw=1"
```

**Viewing a Password-Protected Paste:**
Via Header:
```bash
curl -H "paste-password: YOUR_PASSWORD" https://paste.krfoss.org/<PASTE_ID>
```
Via Query String:
```bash
curl "https://paste.krfoss.org/<PASTE_ID>?password=YOUR_PASSWORD"
```

### 4. Deleting Pastes
When a paste is created, a unique delete token is provided in the response. Use this token to delete the paste.

```bash
curl "https://paste.krfoss.org/<PASTE_ID>?delete=<DELETE_TOKEN>"
```

### 5. Burn After Reading (One-Time View)
To create a paste that deletes itself after the first view, use the `data-policy: once` header.

```bash
echo "temporary secret" | curl -H "data-policy: once" -X POST --data-binary @- https://paste.krfoss.org/
```

### 6. One-Week Retention
To keep a paste for 7 days and then delete it automatically, use the `/week` upload path.

```bash
echo "retained for one week" | curl -X POST --data-binary @- https://paste.krfoss.org/week
```

## Best Practices
- Always use `--data-binary @-` when piping to prevent `curl` from stripping newlines or altering the payload.
- Save the delete token and password (if applicable) immediately after upload.
- The service automatically strips ANSI escape codes when rendering in a browser, ensuring clean text display.
