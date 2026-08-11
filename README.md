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

## Flags

| Flag         | Default          | Description                                              |
| ------------ | ---------------- | -------------------------------------------------------- |
| `--inactive` | `false`          | Include archived projects.                               |
| `-j`         | `min(NCPU, 8)`   | Number of concurrent `git clone`/`git pull` operations.  |

Progress is shown as a progress bar. Projects whose repository you are not
allowed to download are counted as "skipped (no access)" rather than printed
one by one; only real failures are printed. The summary looks like:

```text
Done: 412 synced, 48 skipped (no access), 1 failed.
```

## Rate limiting

GitLab throttles the authenticated API (`throttle_authenticated_api`, 2000
requests per period on gitlab.com). `glab-cloneall` lists projects with
`include_subgroups=true`, so a whole group tree costs one paginated sequence
instead of one request per subgroup. It also reads the `RateLimit-*` response
headers: when the remaining budget gets low it pauses until `RateLimit-Reset`,
and it retries `429` and `5xx`/`Bad Gateway` responses with exponential backoff.

If you route traffic through a local proxy such as mitmproxy, keep `-j` modest —
too many parallel clones can exhaust the proxy's file-descriptor limit
(`OSError: [Errno 24] Too many open files`).
