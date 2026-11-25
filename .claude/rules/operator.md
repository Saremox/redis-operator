---
paths:
  - "operator/**/*.go"
  - "service/**/*.go"
---

- A failover bug can lose data. Read `docs/logic.md` for the mode that you change before you edit the checker or the healer.
- Call the Kubernetes API only through `service/k8s`.
- A new wait or timeout must be a variable that a test can shorten. Tests must not use `time.Sleep` for it.
