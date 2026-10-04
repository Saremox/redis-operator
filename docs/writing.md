# Writing guidelines

These rules apply to the README, the files in `docs/`, Go comments, CRD field descriptions, chart `values.yaml` comments, commit messages and pull request descriptions. Write in [ASD-STE100](https://www.asd-ste100.org/) (Simplified Technical English). Many readers of this project do not speak English as their first language, and many readers are tools.

## What to write

- Write the **why**: the reason for a behaviour, a limit or a decision that the code does not show.
- Do not write the **how** or the **what** when the code, the identifier names or the diff already show them.
- Keep each fact that a user needs, for example a default value, a limit, a risk or a manual step. Make it shorter, but do not remove it.
- Add new documentation only when a reader needs a reason that is missing.
- Remove:
  - comments that repeat the code;
  - history, such as "previously", "now" or "this PR changes";
  - hedging, filler and marketing words.

## Sentences

- Use a maximum of 20 words in an instruction and 25 words in a description.
- Write one topic in each sentence and one topic in each paragraph. Use a maximum of 6 sentences in a paragraph.
- Use the active voice and simple tenses: present, simple past and simple future.
- Do not use the -ing form of a verb, except in technical names.
- Do not use phrasal verbs (for example "set up" or "pick up") or contractions (for example "don't").
- Use words with one meaning. Use the same term for the same thing everywhere.
- Use articles ("a", "the") where possible.
- Write a procedure as numbered steps, one instruction in each step. Start each step with a verb in the imperative.
- Use technical names as they are: identifiers, Redis commands, Kubernetes terms, flags and field names. Put them in backticks.

## Go comments

- Start a doc comment with the name of the identifier.
- Use one or two sentences. Give the reason, not a repeat of the signature.
- A comment that only repeats the signature is noise. Remove it, unless a linter requires it.
- In code, add a comment only where the reason is not clear from the code.

## CRD field descriptions

The CRD descriptions come from the doc comments in `api/redisfailover/v1/types.go`. After you change such a comment:

1. Run `make generate-api`.
2. Stage the changes: `git add api/ manifests/ charts/redisoperator/crds/`. `make verify-codegen` compares with the staged files.
3. Run `make verify-codegen`.
4. Commit the changed CRD files together with the comment.

## Pull request descriptions

1. Start with `Fixes # .` and the list `Changes proposed on the PR:`, as in the pull request template.
2. Write one item for each change, and give the reason for it.
3. Name the tests. Name the tests that fail on `main` without the change.
4. Keep the evidence: measurements, tables and log lines from test runs. Make the text around them short.
5. State what was not tested.

## Releases

When a release needs a manual step, add a migration guide. See [docs/migrations](migrations/README.md).
