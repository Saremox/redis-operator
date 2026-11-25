---
paths:
  - "**/*_test.go"
---

- Use the external `_test` package. Use an internal package only for unexported names, and then end the file name in `_internal_test.go`.
- Use a table test with `t.Run` for cases that differ in data only. Do not compare a whole generated object when one field is the point of the test.
- Do not use `time.Sleep`. Use a short injectable timeout or `require.Eventually`.
- Name a file after the behaviour that it tests, not after a review round.
- Do not start a test file above 800 lines.
