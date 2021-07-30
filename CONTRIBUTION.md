# How to contribute

Windows Machine Config Operator is Apache 2.0 licensed and accepts contributions via GitHub pull requests. This
document outlines some of the conventions on commit message formatting and other resources to help get contributions into the project.  


## Reporting bugs and creating issues

If any part of the project has bugs or documentation mistakes, please let us know by opening a
[Jira issue](https://jira.coreos.com/projects/WINC/summary)

## Contribution flow

This is an outline of what a contributor's workflow looks like:

- Fork the repository on GitHub.
- Clone the forked repository outside your go path, or refer to the [Kubernetes repo's example](https://github.com/kubernetes/community/blob/master/contributors/guide/github-workflow.md#2-clone-fork-to-local-storage).
- Create a topic branch, branched off of master.
- Make commits of logical units. A commit should typically add a feature or fix a bug, but never both at the same
time. Vendor commits should always be separate.
- A PR should consist of a set of logically connected commits to the issue that makes it easy to review and should not 
follow your personal development flow.
- Make sure commit messages are in the proper format (see below).
- Push changes in a topic branch to a personal fork of the repository.
- To make sure that your topic branch is in sync with the remote master branch,
follow a [rebase workflow](https://www.atlassian.com/git/tutorials/merging-vs-rebasing).
- Submit a pull request to openshift/windows-machine-config-operator (see PR workflow below).
- The PR must receive one `/lgtm` and one `/approve` comments from the maintainers of the project.

Thanks for contributing!

### Format of the pull request (PR)

- The PR header for a feature should be prefixed with the Jira issue. Example: `WINC-123:`
- The PR header for a bug should be prefixed with the Bugzilla number. Example: `Bug 123:`
- Correctly prefixing the PR header will automatically associate the PR with the Jira issue or Bugzilla bug.
  - In the case of bug fix PRs, this will also automatically transition the associated Bugzilla bug.
    - Opening the PR: `ASSIGNED` --> `POST`
    - PR merges: `POST` --> `MODIFIED`
    - `MODIFIED` to the `ON_QA` transition will have to be done manually by the PR author.
- The individual commit messages should not be prefixed 

### Format of the commit message

We follow a convention for commit messages that is designed to answer two questions: what changed and why. The
subject line should feature the what and the body of the commit should describe the why.

The format can be described more formally as follows:

```
[subsystem] <what changed>
<BLANK LINE>
<why this change was made>
<BLANK LINE>
<footer>
```
Example for a sample feature:
```
[docs] Add the Guidlines

Cupcake ipsum dolor sit. Amet tart cheesecake tiramisu chocolate cake topping.
Icing ice cream sweet roll. Biscuit dragée toffee wypas.

Follow-up to Id5e7cbb1.
```

The first line is the subject and should be no longer than 50 characters, the second line is always blank, and other
lines should be wrapped at 80 characters. This allows the message to be easier to read on GitHub as well as in various
git tools.

If it is a bug fix commit, the bug number should be mentioned in the commit message as `fixes BZ#123` in a separate
line.

### PR workflow

- Before submitting a PR
  - Format your code and fix all the spelling/grammatical mistakes.
  - Limit the column length of the code and the comments within the code to 120 characters.
  - Error messages within the code should be limited to a single line.
  - Update the documentation if your PR is introducing or changing user facing functionality.
  - Ensure each PR commit compiles and all unit and e2e tests pass on your local machine.
    - PRs that have vendor commits are an exception to this rule.
- Following are the things you should keep in mind once you open a PR:
  - Add a hold as soon as you open the PR by commenting `/hold`
  - Wait for comments from at least 2 reviewers before pushing changes.
  Open comments in the meanwhile can be worked on locally.
  - PR comments should be addressed in new commits. Before final approval, they have to be squashed.
  - If PR has multiple commits, changes requested should eventually be squashed into the original commit where the
  change was requested before cancelling the hold. Each commit in the final PR before merge should pass the tests and be
  usable. (see below)
  - Once the PR is approved, remove the hold by commenting `/hold cancel` to merge the PR

![Sample PR life-cycle](/images/PR-workflow.png)
