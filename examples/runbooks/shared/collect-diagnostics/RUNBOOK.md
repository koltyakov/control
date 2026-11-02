---
title: Collect machine diagnostics
description: Inspect a requested machine's Control status and cached resources.
---

# Collect machine diagnostics

Use when the user asks for a machine's current Control status or resource information.

This is a shared runbook. Select the machine requested by the user, never a substitute.

1. Resolve its identity, User/System context, availability, and policy with `control machines`.
2. Read capability schemas and applicable runbooks with `control call NAME node.describe`.
3. Read cached resources with `control system NAME`. Do not request a refresh unless needed.
4. Read bounded operational counters with `control diagnostics NAME` only when requested and authorized.
5. Summarize observations and their sample times. Distinguish missing readings and source errors from zero usage or absent software.

Do not read credentials, execution arguments, clipboard contents, or unrelated files. Do not install tools, wake a machine, or switch to System as a side effect of observation. If a read fails, report it without replaying unrelated work.
