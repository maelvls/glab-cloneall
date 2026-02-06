# glab-cloneall

Clones and syncs all repos under a certain GitLab group.

## Install

> [!NOTE]
>
> You will need to have `glab` installed and logged in to use
> `glab-cloneall`.

```bash
go install github.com/maelvls/glab-cloneall@latest
```

## Usage

[demo.webm](https://github.com/user-attachments/assets/2a55508a-fda2-4d33-84f2-62a266d932cc)

For example, the command:

```bash
glab-cloneall https://gitlab.com/gitlab-org/go
```

will clone all repos under the `go` group. You will end up with the following
structure:

```text
.
└── go
    ├── icu
    └── reopen
```

If you re-run the command, it will pull the latest changes for each repo instead
of cloning them again.

You can also specify a target directory as the second argument. For example:

```bash
glab-cloneall https://gitlab.com/gitlab-org/go /tmp/go
```
