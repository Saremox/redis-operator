# Migration guides

The [GitHub releases](https://github.com/Saremox/redis-operator/releases) list the changes in each release. A migration guide in this folder gives the manual steps for an upgrade.

## When a release needs a guide

Add a guide when an upgrade to the release needs one of these:

- A manual step. For example: re-apply the CRD, change RBAC, or change a RedisFailover manifest.
- A behaviour change that the user sees during the upgrade. For example: the operator restarts the pods, or the API server removes fields.

Do not add a guide for changes that need no action. The release notes cover them.

## File name

Name the file `<version>.md`, without a leading `v` and without the release candidate suffix. For example: `4.2.0.md`.

The guide for `4.2.0` also applies to its release candidates, for example `4.2.0-rc3`. Add the guide before you push the first tag that needs it.

## Release notes

The release workflow looks for `docs/migrations/<version>.md` at the tag. If the file exists, the workflow puts a warning at the top of the release notes. The warning links to the guide at the tag.

## Content

- Say which upgrades the guide applies to.
- Give each step as a numbered procedure.
- Give the reason for each step.
- Give a check that the user can do after the upgrade.
- Do not copy the list of changes from the release notes.
