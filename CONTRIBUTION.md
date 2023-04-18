# How to contribute

Windows Machine Config Operator is Apache 2.0 licensed and accepts contributions via GitHub pull requests. This
document outlines some of the conventions on commit message formatting and other resources to help get
contributions into the project.  

## New Contributor guide

To get an overview of what the WMCO does, please take a look at the [README](README.md) first. In order to
work with the codebase, you'll need to understand a few basic git concepts. Follow the steps below to set up your
environment to get started.

- [Fork the repository on GitHub.](https://docs.github.com/en/get-started/quickstart/fork-a-repo)
- [Clone the forked repository outside your go path](https://docs.github.com/en/repositories/creating-and-managing-repositories/cloning-a-repository)
- [Create a topic branch, branched off of master.](https://www.atlassian.com/git/tutorials/using-branches)
- [Make commits of logical units as you work](https://github.com/git-guides/git-commit)

## Before opening a pull request

Thank you for contributing to the Windows Machine Config Operator!

Before opening a pull request, be sure that your changes have been tested, documented, and checked for style. Once
you think your PR is ready for review, be sure to check that you have

- [Fetched from upstream WMCO](https://docs.github.com/en/get-started/using-git/getting-changes-from-a-remote-repository#fetching-changes-from-a-remote-repository)
- [Rebased your changes against your root branch](https://www.atlassian.com/git/tutorials/merging-vs-rebasing)
- [Run the tests locally](docs/HACKING.md)
- Linted your changes with ```make lint```
- Ensured error messages are a single line
- Updated relevant documentation if your PR changes user functionality  

### Format of the commit message

We follow a convention for commit messages that is designed to answer two questions: what changed and why. The
subject line should feature the what and the body of the commit should describe the why.

The format can be described more formally as follows:

```
[subsystem] <what changed>
<BLANK LINE>
<why this change was made>
<BLANK LINE>
<Footer>
```

A real world example would look like

```
[docs] Add the guidelines

The contribution guidelines were not aligned with current practices. Update the
guidelines and reorganize it to bring it up to date.

Follow-up to Id5e7cbb1.
```

The subject should be no longer than 50 characters, and the body should be no longer than 80 characters. There are some
[githooks](https://github.com/jorisroovers/gitlint) you can install that can help enforce these style requirements.

## Opening a pull request

Once you are done with the pre-PR checklist, push your changes and create a pull request!

### Format of the pull request

#### Title

When creating your pull request, you should prefix it with the Jira issue, followed by the subsystem name in brackets. It
should follow the format

```WINC|OCPBUGS-<number>: [<subsystem>] <title>```

For example, a docs PR would look like

```WINC-959: [docs] reorganizes readme```

In doing this, you’ll be linking your PR to the Jira ticket for tracking.

In the case of bug fix PRs, this will also automatically transition the associated Jira bug.

- Opening the PR: `ASSIGNED` --> `POST`
- PR merges: `POST` --> `MODIFIED`
- `MODIFIED` to the `ON_QA` transition will have to be done manually by the PR author.
- PR closes without merging, the Jira bug transitions back to NEW status.

If you have no Jira issue or are not a Red Hat employee, your title should simply follow the format `[<subsystem>] <title>`.

If your work is linked to a GitHub issue, add `Fixes #<issue>` in the comment.

### Open as draft

The WMCO team opens pull requests as drafts before they are reviewed in order to prevent the tests from running right away
[Open your PR as a draft](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/proposing-changes-to-your-work-with-pull-requests/creating-a-pull-request)
before requesting reviews.

## Merging a pull request

Once your PR is open, you'll need at least two reviewers te get it merged.

### Moving your PR out of draft

Before moving your PR out of draft, you must have at least one /lgtm and one /approve on the PR.
Once you have both, you are free to move it out of draft status by clicking the "ready for review" button
in your PR interface. This will let your tests start running, and if they pass, your PR will merge automatically.

### Respond to failing tests

If your PR is experiencing a test failure that you believe to be a flake, feel free to run a retest.
When retesting, be sure to add a link to the failing test, and a snippet of what caused the failure.
The format should be

```
/retest-required

<explanation of the error>
<reason for retest>
<prow.ci.openshift.org link>

<log snippet of the failure>
```

Once your tests are passing, and your PR has an approval and a LGTM label, the CI should merge it automatically.

![Sample PR life-cycle](/images/PR-workflow.png)

## Backports

When backporting changes, use the openshift-cherrypick-robot whenever possible to auto-generate backports.
Once your PR has merged, go to the PR and comment `/cherry-pick <release>` with whatever 
[supported](https://access.redhat.com/support/policy/updates/openshift#windows)
releases you want to backport to.

## Backporting bugfixes

Bugfixes should be backported to all 
[supported versions](https://access.redhat.com/support/policy/updates/openshift#windows).
Once your bugfix is merged, go to the merged PR and comment
`/cherry-pick <release>`
to trigger the openshift-cherrypick-robot and create an automated backport.
If you have multiple versions you need to backport to, go to each generated backport and run the
`/cherry-pick <release>` on that instead of master. This ensures the Jira bugs are linked properly.

If for example your PR to master needed to be backported to 4.11, 4.10, and 4.9, you would create an automatic
backport against 4.11, go to that generated backport, and from there run `/cherry-pick 4.10`. Then, from
your 4.10 backport you would generate a backport for 4.9.

If the cherry pick bot fails, you will have to make your cherry picks manually, and execute
/jira cherry-pick OCBUGS-<number> in the manual PR to create the Jira associations.

## Reporting bugs and creating issues

If any part of the project has bugs or documentation mistakes, please let us know by opening a
[GitHub issue](https://github.com/openshift/windows-machine-config-operator/issues)
