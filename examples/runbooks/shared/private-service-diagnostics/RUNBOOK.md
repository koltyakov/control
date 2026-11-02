---
title: Diagnose a private service
description: Check a service from a machine with access to the intended environment.
requires: >
  A machine that can reach the requested service through the appropriate network
  or VPN. If the task needs a particular vendor's VPN, choose a machine with
  that client and environment configured.
---

# Diagnose a private service

Use a machine with suitable access. The VPN client, network setup, and operating system depend on the user's environment rather than this shared runbook.

1. Establish which service and environment the user wants checked.
2. Choose a suitable machine and inspect its relevant network or VPN access. Consult its machine-specific runbook when configuration details are needed.
3. Perform the requested diagnostic against the authorized service URL.
4. Report the machine used, observations, and result, including anything that could not be verified.

Keep credentials on the worker and use secret references when needed. Ask before changing network configuration or installing software outside the requested task.
