# Built-in signatures

<!-- Generated from internal/signatures/builtin/*.yml by `just docs-signatures`. Do not edit by hand. -->

rx has 70 built-in signatures in 11 groups. 64 of them are in the default set, which
`rx --signatures` runs; the others are opt-in and run when they or their group are
named, as in `--signatures=default,pii`. The model and the file format are in
[Signatures](../concepts/signatures.md); `rx signatures show ID` prints one definition in
full, with its regexes and examples.

Keywords are the literals a line must hold before the signature's regex is tried on it,
compared without case: a line needs one keyword of each set, and the sets are joined by
"and" below. A signature without keywords is tried on every line.

| Group | Signatures | In the default set | What it finds |
|---|---|---|---|
| [traceback](#traceback) | 8 | 8 | Stack traces and uncaught exceptions of programs in many languages. |
| [crash](#crash) | 9 | 9 | Crashes, aborts, sanitizer reports, kernel oopses and processes the system killed. |
| [secret](#secret) | 16 | 16 | Credentials and keys written into a log. Their values are masked in every output. |
| [pii](#pii) | 5 | 0 | Personal data such as e-mail addresses, IP addresses, card numbers and IBANs. |
| [sql](#sql) | 7 | 7 | SQL statements, dangerous statements, query plans, slow queries and lock problems. |
| [http](#http) | 1 | 1 | HTTP requests answered with a server error. |
| [severity](#severity) | 2 | 1 | Lines written at an error or warning level. |
| [system](#system) | 7 | 7 | Resource and network failures such as a full disk, too many open files, timeouts, TLS and DNS errors. |
| [lifecycle](#lifecycle) | 3 | 3 | Signals, exit statuses and service state changes. |
| [structure](#structure) | 9 | 9 | The shape of the text, such as runs of blank or repeated lines, long lines, JSON blocks and binary bytes. |
| [time](#time) | 3 | 3 | The timing of the log, such as bursts of lines, silences and timestamps that go backwards. |

## traceback

Stack traces and uncaught exceptions of programs in many languages.

### python-traceback

**Python traceback** · level `high` · default · block

An uncaught Python exception: the "Traceback (most recent call last):" header at the start of a line, the indented frames, and the exception line that ends it. Chained exceptions ("During handling of the above exception..." and "The above exception was the direct cause...") and exception groups ("Exception Group Traceback", with their "\|" and "+" margins) stay in one finding. False positives are rare: the header is specific. A traceback whose lines carry a log prefix (a timestamp before "Traceback") is not found.

- Keywords: `"traceback (most recent call last):"` or `"exception group traceback (most recent call last):"` (derived from the regex)
- Tags: `python`
- Links: <https://docs.python.org/3/library/traceback.html>,
  <https://raw.githubusercontent.com/python/cpython/main/Lib/traceback.py>,
  <https://raw.githubusercontent.com/GoogleCloudPlatform/fluent-plugin-detect-exceptions/master/lib/fluent/plugin/exception_detector.rb>

### java-exception

**Java exception** · level `high` · default · block

A JVM exception with its stack trace: a line that starts with a dotted class name ending in Exception, Error or Throwable (optionally after 'Exception in thread "name"'), then at least one "at" frame, with "Caused by:", "Suppressed:" and "... N more" lines kept in the same finding. An exception line without frames is not a finding, so a message that only names an exception class is not reported. Kotlin and Scala print the same shape.

- Keywords: `"exception:"` or `"exception"` or `"error:"` or `"error"` or `"throwable:"` or `"throwable"` (derived from the regex)
- Tags: `java`, `jvm`
- Links: <https://docs.oracle.com/en/java/javase/21/docs/api/java.base/java/lang/Throwable.html>,
  <https://raw.githubusercontent.com/GoogleCloudPlatform/fluent-plugin-detect-exceptions/master/lib/fluent/plugin/exception_detector.rb>,
  <https://www.elastic.co/guide/en/beats/filebeat/current/multiline-examples.html>

### dotnet-exception

**.NET exception** · level `high` · default · block

A .NET exception with its stack trace: a line that starts with a capitalised dotted type name ending in "Exception: " (optionally after "Unhandled exception. "), then at least one "at" frame, with inner exceptions (" ---&gt; ") and "--- End of ... stack trace ---" separators kept in the same finding. An exception line without frames is not a finding.

- Keywords: `"exception: "` (derived from the regex)
- Tags: `dotnet`, `csharp`
- Links: <https://learn.microsoft.com/en-us/dotnet/api/system.exception.stacktrace>,
  <https://raw.githubusercontent.com/GoogleCloudPlatform/fluent-plugin-detect-exceptions/master/lib/fluent/plugin/exception_detector.rb>

### js-error

**JavaScript error** · level `high` · default · block

A JavaScript error with its V8 stack trace: a line that starts with an error name ending in "Error: " (optionally after "Uncaught " or "UnhandledPromiseRejectionWarning: "), then at least one " at ..." frame. An error line without frames is not a finding. V8 keeps ten frames by default.

- Keywords: `"error"` (derived from the regex)
- Tags: `javascript`, `node`
- Links: <https://v8.dev/docs/stack-trace-api>,
  <https://raw.githubusercontent.com/GoogleCloudPlatform/fluent-plugin-detect-exceptions/master/lib/fluent/plugin/exception_detector.rb>

### go-panic

**Go panic** · level `high` · default · block

A Go panic or fatal runtime error with its goroutine dump: "panic:", "fatal error:" or "http: panic serving", then the goroutine headers (also the "goroutine N gp=... m=... \[status\]:" form the runtime prints while throwing), the frames, "created by" lines and the blank lines between goroutines, up to "exit status N". A stand-alone goroutine dump is found from its first goroutine header. A "panic:" line with nothing after it is not a finding, which keeps out C compilers' "fatal error:" lines.

- Keywords: `"panic: "` or `"fatal error: "` or `"sigquit: quit"` or `"sigabrt: abort"` or `"http: panic serving "` or `"goroutine "` (derived from the regex)
- Tags: `go`
- Links: <https://pkg.go.dev/runtime>,
  <https://raw.githubusercontent.com/golang/go/master/src/runtime/panic.go>,
  <https://raw.githubusercontent.com/golang/go/master/src/runtime/traceback.go>,
  <https://raw.githubusercontent.com/golang/go/master/src/net/http/server.go>

### ruby-backtrace

**Ruby backtrace** · level `high` · default · block

An uncaught Ruby exception: "file.rb:LINE:in 'method': message (Class)" at the start of a line, then at least one indented "from file:LINE:in ..." frame. Both quote styles are found: the backtick of Ruby before 3.4 and the single quote of 3.4 and later.

- Keywords: `".rb:"`; and `":in "`; and `"': "` (derived from the regex)
- Tags: `ruby`
- Links: <https://www.ruby-lang.org/en/news/2024/12/25/ruby-3-4-0-released/>,
  <https://raw.githubusercontent.com/GoogleCloudPlatform/fluent-plugin-detect-exceptions/master/lib/fluent/plugin/exception_detector.rb>

### php-fatal

**PHP fatal error** · level `high` · default · block

A PHP fatal or parse error, with its "Stack trace:" and "\#N" frames and the "thrown in" line when PHP printed them, or an uncaught exception in the older "exception 'Class' with message" form. A fatal error without a trace is a finding of one line. PHP warnings and notices are not fatal and are not found.

- Keywords: `"php fatal error: "` or `"php parse error: "` or `"' with message '"` (derived from the regex)
- Tags: `php`
- Links: <https://raw.githubusercontent.com/GoogleCloudPlatform/fluent-plugin-detect-exceptions/master/lib/fluent/plugin/exception_detector.rb>

### rust-panic

**Rust panic** · level `high` · default · block

A Rust panic: "thread 'name' panicked at file:line:col" (the thread id in parentheses that newer releases print included), the message on the next line in the current format, the "note:" lines and, with RUST\_BACKTRACE set, the "stack backtrace:" and its numbered frames. The older format, with the message inside the header line, is found too.

- Keywords: `" panicked at "` (derived from the regex)
- Tags: `rust`
- Links: <https://doc.rust-lang.org/book/ch09-01-unrecoverable-errors-with-panic.html>

## crash

Crashes, aborts, sanitizer reports, kernel oopses and processes the system killed.

### sanitizer-report

**Sanitizer report** · level `critical` · default · block

A report of AddressSanitizer, LeakSanitizer, MemorySanitizer, ThreadSanitizer or UndefinedBehaviorSanitizer: from the "==PID==ERROR: ...Sanitizer:" line (or ThreadSanitizer's "WARNING: ThreadSanitizer:") through the "SUMMARY:" line, the shadow-byte map after it and "==PID==ABORTING". A report cut off before its summary runs to max\_lines. False positives are rare.

- Keywords: `"==error: address"` or `"==error: hwaddress"` or `"==error: leak"` or `"==error: memory"` or `"==error: thread"` or `"==error: undefinedbehavior"` or `"warning: threadsanitizer: "` (derived from the regex)
- Tags: `asan`, `sanitizer`, `c`, `cpp`
- Links: <https://github.com/google/sanitizers/wiki/AddressSanitizer>

### glibc-abort

**glibc fatal error** · level `critical` · default · block

A fatal error glibc printed before it aborted the process: "\*\*\* stack smashing detected \*\*\*: terminated", "\*\*\* buffer overflow detected \*\*\*", the older "\*\*\* Error in \`prog'" form, and the heap checks such as "free(): double free detected in tcache 2" or "corrupted size vs. prev\_size". The backtrace and memory map that older releases print, and the shell's "Aborted (core dumped)" after them, are kept in the same finding.

- Keywords: `"terminated"` or `"error in "` or `"free(): "` or `"malloc(): "` or `"realloc(): "` or `"munmap_chunk(): "` or `"malloc_consolidate(): "` or `"_int_free(): "` or `"_int_malloc(): "` or `"double free or corruption"` or `"corrupted size vs. prev_size"` or `"corrupted double-linked list"` (derived from the regex)
- Tags: `glibc`, `c`
- Links: <https://raw.githubusercontent.com/bminor/glibc/master/debug/fortify_fail.c>,
  <https://raw.githubusercontent.com/bminor/glibc/master/debug/stack_chk_fail.c>

### killed-by-signal

**Process killed by a signal** · level `high` · default · block

A shell or supervisor reporting that a process died of a signal: "Segmentation fault", "Killed", "Aborted", "Bus error", "Illegal instruction", "Floating point exception" or "Trace/breakpoint trap", at the start of a line or after ": " and at its end, with "(core dumped)" on the same or the next line; or the shell's job form "script: line N: PID Killed command". "Killed" is a common word, so it is found only in those positions.

- Keywords: `"segmentation fault"` or `"killed"` or `"aborted"` or `"bus error"` or `"illegal instruction"` or `"floating point exception"` or `"trace/breakpoint trap"` or `": segmentation fault"` or `": killed"` or `": aborted"` or `": bus error"` or `": illegal instruction"` or `": floating point exception"` or `": trace/breakpoint trap"` or `": line "` (derived from the regex)
- Tags: `shell`, `signal`
- Links: <https://raw.githubusercontent.com/bminor/glibc/master/sysdeps/generic/siglist.h>

### kernel-segfault

**Kernel segfault report** · level `high` · default · regex

The kernel's report of a user process that touched memory it may not: "prog\[PID\]: segfault at ADDR ip ADDR sp ADDR error N". False positives are rare.

- Keywords: `"]: segfault at "`; and `" ip "`; and `" sp "`; and `" error "` (derived from the regex)
- Tags: `kernel`, `linux`
- Links: <https://raw.githubusercontent.com/torvalds/linux/master/arch/x86/mm/fault.c>

### systemd-coredump

**systemd core dump** · level `high` · default · regex

systemd-coredump recording a process that crashed: "Process PID (name) of user UID dumped core." or "terminated abnormally without generating a coredump.". False positives are rare.

- Keywords: `"process "`; and `") of user "`; and `"dumped core."` or `"terminated abnormally without generating a coredump."` (derived from the regex)
- Tags: `systemd`, `linux`
- Links: <https://raw.githubusercontent.com/systemd/systemd/main/src/coredump/coredump-submit.c>

### kernel-oops

**Kernel oops or panic** · level `critical` · default · block

A kernel oops, BUG or panic: "BUG: kernel NULL pointer dereference", "BUG: unable to handle page fault", "kernel BUG at file:line!", "Oops: ... \[\#N\]", "Kernel panic - not syncing:", or a "Call Trace:" of its own, through the "---\[ end trace ... \]---" line. Lines join while they carry the kernel's bracketed time or a "kernel:" tag, or look like an oops body (registers, frames, "Code:"). In dmesg or the journal every kernel line carries one of those, so an oops without its end line runs on through the kernel lines after it, up to 200 lines.

- Keywords: `"kernel null pointer dereference"` or `"unable to handle "` or `"kernel bug at "` or `"oops: "` or `"kernel panic - not syncing: "` or `"call trace:"` (derived from the regex)
- Tags: `kernel`, `linux`
- Links: <https://raw.githubusercontent.com/torvalds/linux/master/arch/x86/kernel/dumpstack.c>,
  <https://raw.githubusercontent.com/torvalds/linux/master/kernel/panic.c>,
  <https://raw.githubusercontent.com/torvalds/linux/master/arch/x86/mm/fault.c>

### oom-killer

**OOM killer** · level `high` · default · block

The kernel's out-of-memory killer at work: from "PROG invoked oom-killer: gfp\_mask=" through the "Out of memory: Killed process PID" (or "Memory cgroup out of memory: Killed process") line, with the memory report between them. Lines join while they carry the kernel's bracketed time, a "kernel:" tag or an indent.

- Keywords: `" invoked oom-killer: gfp_mask="` (derived from the regex)
- Tags: `kernel`, `linux`, `memory`
- Links: <https://raw.githubusercontent.com/torvalds/linux/master/mm/oom_kill.c>

### k8s-crash-loop

**Kubernetes container restarts** · level `high` · default · regex

A Kubernetes container that keeps failing: "CrashLoopBackOff", the kubelet's "Back-off restarting failed container" event, or the termination reason "OOMKilled". False positives are rare.

- Keywords: `"crashloopbackoff"` or `"back-off restarting failed container"` or `"oomkilled"` (derived from the regex)
- Tags: `kubernetes`
- Links: <https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/>,
  <https://raw.githubusercontent.com/kubernetes/kubernetes/master/pkg/kubelet/kuberuntime/kuberuntime_manager.go>

### jvm-fatal-error

**JVM fatal error** · level `critical` · default · block

The banner the JVM prints when it crashes in native code or runs out of native memory: "\# A fatal error has been detected by the Java Runtime Environment:" or "\# There is insufficient memory for the Java Runtime Environment to continue.", with the "\#" lines that follow it (the signal, the JRE version and the name of the hs\_err file).

- Keywords: `"a fatal error has been detected by the java runtime environment"` or `"there is insufficient memory for the java runtime environment to continue"` (derived from the regex)
- Tags: `java`, `jvm`
- Links: <https://docs.oracle.com/javase/8/docs/technotes/guides/troubleshoot/felog.html>

## secret

Credentials and keys written into a log. Their values are masked in every output.

### aws-access-key

**AWS access key ID** · level `high` · default · regex · values masked

An AWS access key ID: a four-character prefix that names the kind of key (AKIA for a long-term key, ASIA for a temporary one) and sixteen more characters. The key ID alone does not sign requests, but it names the account and usually travels with its secret. The example IDs of AWS documentation, which end in EXAMPLE, are found too.

- Keywords: `"a3t"` or `"akia"` or `"asia"` or `"abia"` or `"acca"` (derived from the regex)
- Tags: `aws`, `cloud`
- Links: <https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_identifiers.html>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### aws-secret-key

**AWS secret access key** · level `critical` · default · regex · values masked

An AWS secret access key: 40 base64 characters within 20 characters after a name that says "aws" and "secret", "key", "password" or "token" (aws\_secret\_access\_key=..., "AWSSecretKey": "..."). The value is masked. A 40-character value that is not a key but sits after such a name is found too.

- Keywords: `"aws"` (derived from the regex)
- Tags: `aws`, `cloud`
- Links: <https://raw.githubusercontent.com/Yelp/detect-secrets/master/detect_secrets/plugins/aws.py>,
  <https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_access-keys.html>

### github-token

**GitHub token** · level `high` · default · regex · values masked

A GitHub token: a personal access token (ghp\_), an OAuth token (gho\_), a user-to-server or server-to-server token (ghu\_, ghs\_), a refresh token (ghr\_), each with 36 more characters, or a fine-grained personal access token (github\_pat\_ and 82 more). False positives are rare: the prefixes are specific.

- Keywords: `"ghp_"` or `"gho_"` or `"ghu_"` or `"ghs_"` or `"ghr_"` or `"github_pat_"` (declared)
- Tags: `github`
- Links: <https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/about-authentication-to-github#githubs-token-formats>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### gitlab-token

**GitLab personal access token** · level `high` · default · regex · values masked

A GitLab personal access token: "glpat-" and at least 20 more characters. False positives are rare.

- Keywords: `"glpat-"` (derived from the regex)
- Tags: `gitlab`
- Links: <https://docs.gitlab.com/ee/security/tokens/>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### slack-token

**Slack token** · level `high` · default · regex · values masked

A Slack API token: a bot, user, app, refresh or configuration token ("xoxb-", "xoxp-", "xoxa-", "xoxe-", "xoxo-", "xoxr-", "xoxs-" and ten or more characters) or an app-level token ("xapp-"). False positives are rare.

- Keywords: `"xox"` or `"xapp-"` (derived from the regex)
- Tags: `slack`
- Links: <https://api.slack.com/authentication/token-types>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### slack-webhook

**Slack webhook URL** · level `high` · default · regex · values masked

A Slack incoming-webhook, workflow or trigger URL (hooks.slack.com/services/...): anyone who holds it can post to the channel. The path after the kind is the value that is masked.

- Keywords: `"hooks.slack.com/services/"` or `"hooks.slack.com/workflows/"` or `"hooks.slack.com/triggers/"` (derived from the regex)
- Tags: `slack`
- Links: <https://api.slack.com/messaging/webhooks>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### stripe-key

**Stripe API key** · level `high` · default · regex · values masked

A Stripe secret or restricted API key: "sk\_" or "rk\_", then "live\_", "test\_" or "prod\_", then 10 to 99 characters. Test keys are found too; they cannot move money but still belong to an account. Publishable keys ("pk\_") are public and not found.

- Keywords: `"sk_test_"` or `"sk_live_"` or `"sk_prod_"` or `"rk_test_"` or `"rk_live_"` or `"rk_prod_"` (derived from the regex)
- Tags: `stripe`, `payments`
- Links: <https://docs.stripe.com/keys>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### google-api-key

**Google API key** · level `high` · default · regex · values masked

A Google Cloud or Firebase API key: "AIza" and 35 more characters. Some of these keys are meant to sit in web pages; they are found all the same.

- Keywords: `"aiza"` (derived from the regex)
- Tags: `google`, `cloud`
- Links: <https://cloud.google.com/docs/authentication/api-keys>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### anthropic-key

**Anthropic API key** · level `high` · default · regex · values masked

An Anthropic API or admin key: "sk-ant-api" or "sk-ant-admin" with two digits, then 80 or more characters. False positives are rare.

- Keywords: `"sk-ant-api"` or `"sk-ant-admin"` (derived from the regex)
- Tags: `anthropic`, `ai`
- Links: <https://docs.anthropic.com/en/api/getting-started>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### openai-key

**OpenAI API key** · level `high` · default · regex · values masked

An OpenAI API key: "sk-" (with "proj-", "svcacct-" or "admin-" in newer keys) and characters around the "T3BlbkFJ" marker that every such key holds. False positives are rare.

- Keywords: `"sk-"`; and `"t3blbkfj"` (derived from the regex)
- Tags: `openai`, `ai`
- Links: <https://platform.openai.com/docs/api-reference/authentication>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### jwt

**JSON Web Token** · level `high` · default · regex · values masked

A signed JSON Web Token: three base64url parts joined by dots, the first two starting with "eyJ" (an encoded JSON object). Expired tokens are found too; a token without its signature part is not.

- Keywords: `".eyj"` (derived from the regex)
- Tags: `jwt`, `auth`
- Links: <https://datatracker.ietf.org/doc/html/rfc7519>,
  <https://raw.githubusercontent.com/Yelp/detect-secrets/master/detect_secrets/plugins/jwt.py>

### private-key

**Private key** · level `critical` · default · block · values masked

A private key in PEM or PGP armour, from its "-----BEGIN ... PRIVATE KEY-----" line through the "-----END ... PRIVATE KEY-----" line (RSA, EC, DSA, OpenSSH, encrypted PKCS\#8 and PGP key blocks), or both markers on one line, as in an escaped JSON value. Every line of the body is masked as its length wherever a finding's text shows it. A key whose END line is missing runs to max\_lines, all of it masked.

- Keywords: `"-----begin"`; and `"private key"` (derived from the regex)
- Tags: `pem`, `tls`, `ssh`, `pgp`
- Links: <https://datatracker.ietf.org/doc/html/rfc7468>,
  <https://raw.githubusercontent.com/Yelp/detect-secrets/master/detect_secrets/plugins/private_key.py>

### password-assignment

**Password assignment** · level `high` · default · regex · values masked

A value assigned to a name such as password, passwd, secret, api\_key, access\_token, auth\_token or client\_secret, with "=" or ":" and optional quotes, at least eight characters long. The value is masked. Placeholders that start with "$", "&lt;", "\*", "{" or "\[" (${DB\_PASSWORD}, &lt;hidden&gt;, \*\*\*\*\*\*\*\*) are not found. This is the noisiest secret signature: an English sentence that puts a colon and a word of eight letters or more after "password" is found too.

- Keywords: `"password"` or `"passwd"` or `"secret"` or `"apikey"` or `"api_key"` or `"api-key"` or `"accesstoken"` or `"access_token"` or `"access-token"` or `"authtoken"` or `"auth_token"` or `"auth-token"` (declared)
- Tags: `password`
- Links: <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>,
  <https://raw.githubusercontent.com/Yelp/detect-secrets/master/detect_secrets/filters/heuristic.py>

### url-password

**Password in a URL** · level `high` · default · regex · values masked

A URL with a user and a password between the scheme and the host, as in database connection strings. The password is masked. Templates whose password starts with "$", "&lt;", "{" or "\*" are not found.

- Keywords: `"://"` (derived from the regex)
- Tags: `password`, `url`
- Links: <https://raw.githubusercontent.com/Yelp/detect-secrets/master/detect_secrets/plugins/basic_auth.py>,
  <https://datatracker.ietf.org/doc/html/rfc3986#section-3.2.1>

### authorization-header

**Authorization header** · level `high` · default · regex · values masked

An HTTP Authorization header or field with its credentials: Bearer, Basic, Token, Digest or Negotiate and a value of at least eight characters. The value is masked. A header whose value is already masked ("Bearer \*\*\*") is not found.

- Keywords: `"authorization"`; and `"bearer"` or `"basic"` or `"token"` or `"digest"` or `"negotiate"` (derived from the regex)
- Tags: `http`, `auth`
- Links: <https://datatracker.ietf.org/doc/html/rfc9110#name-authorization>,
  <https://raw.githubusercontent.com/gitleaks/gitleaks/master/config/gitleaks.toml>

### high-entropy-token

**High-entropy token** · level `medium` · default · measure `entropy` (min_length 20, ratio 0.94, hex_ratio 0) · values masked

A token of letters, digits and + / = \_ -, at least 20 characters long, as varied as a random key: it holds an upper-case letter, a lower-case letter and a digit, and its Shannon entropy is at least 0.94 of what a random token of its length has on average. Nine random keys in ten or more of 24 to 64 characters are found. Hex tokens are left alone, since hashes and request ids are as random as hex keys; UUIDs and numbers are skipped. The token is masked. Random ids and encoded values in a log look the same as keys to this test: base64 of a short English text (14 to 48 bytes) is found about half to two thirds of the time, and about 1 in 100 identifiers that mix upper- and lower-case letters and digits is found.

- Keywords: none: tried on every line
- Tags: `entropy`
- Links: <https://raw.githubusercontent.com/Yelp/detect-secrets/master/detect_secrets/plugins/high_entropy_strings.py>,
  <https://raw.githubusercontent.com/Yelp/detect-secrets/master/detect_secrets/filters/heuristic.py>

## pii

Personal data such as e-mail addresses, IP addresses, card numbers and IBANs.

### email-address

**E-mail address** · level `low` · opt-in · regex

An e-mail address: a local part, "@", and a domain with a dot. user@host forms of SSH logs and git authors are found too.

- Keywords: none: tried on every line
- Tags: `email`
- Links: <https://datatracker.ietf.org/doc/html/rfc5322#section-3.4.1>

### ipv4-address

**IPv4 address** · level `low` · opt-in · regex

An IPv4 address: four numbers from 0 to 255 joined by dots, checked as an address. Version strings with four parts (1.2.3.4) look the same and are found too.

- Keywords: none: tried on every line
- Tags: `ip`, `network`
- Links: <https://raw.githubusercontent.com/logstash-plugins/logstash-patterns-core/main/patterns/ecs-v1/grok-patterns>

### ipv6-address

**IPv6 address** · level `low` · opt-in · regex

An IPv6 address in full (eight groups) or with "::", checked as an address. Times such as 12:34:56 and MAC addresses do not have the shape. The first candidate on a line decides: when it is not an address, the line is not a finding.

- Keywords: none: tried on every line
- Tags: `ip`, `network`
- Links: <https://datatracker.ietf.org/doc/html/rfc4291#section-2.2>

### card-number

**Payment card number** · level `high` · opt-in · regex

A payment card number: 13 to 19 digits, in groups or not, that start like a Visa, Mastercard, American Express or Discover number and pass the Luhn check. About one digit string in ten passes the check by chance, so order numbers with those first digits are found too.

- Keywords: none: tried on every line
- Tags: `payment`, `card`
- Links: <https://en.wikipedia.org/wiki/Luhn_algorithm>,
  <https://en.wikipedia.org/wiki/Payment_card_number>

### iban

**IBAN** · level `high` · opt-in · regex

An international bank account number: two letters, two check digits and up to 30 letters and digits, in groups of four or not, checked by its country's length and the mod-97 rule.

- Keywords: none: tried on every line
- Tags: `payment`, `bank`
- Links: <https://en.wikipedia.org/wiki/International_Bank_Account_Number>

## sql

SQL statements, dangerous statements, query plans, slow queries and lock problems.

### sql-statement

**SQL statement** · level `info` · default · regex

A line that holds an SQL statement: SELECT with a column list and FROM a table, INSERT INTO a table with VALUES or a column list, UPDATE a table SET a column, DELETE FROM a table, CREATE, ALTER or DROP of a table, index, view, schema or database, or TRUNCATE TABLE, with spaces or tabs between the words. Case does not matter. The shapes keep out most English ("select a file from the list"), but a sentence such as "select one from the menu" is still found.

- Keywords: `"select "` or `"select\t"` or `"insert "` or `"insert\t"` or `"update "` or `"update\t"` or `"delete "` or `"delete\t"` or `"create "` or `"create\t"` or `"alter "` or `"alter\t"` or `"drop "` or `"drop\t"` or `"truncate "` or `"truncate\t"` (declared)
- Tags: `sql`
- Links: <https://www.postgresql.org/docs/current/runtime-config-logging.html>

### dangerous-sql

**Dangerous SQL statement** · level `high` · default · regex

A statement that removes data wholesale: DROP TABLE, DATABASE or SCHEMA, TRUNCATE, or a DELETE or UPDATE on a line without WHERE. A statement whose WHERE clause is on a later line of the log is found too, since only its first line is read.

- Keywords: `"drop "` or `"drop\t"` or `"truncate "` or `"truncate\t"` or `"delete "` or `"delete\t"` or `"update "` or `"update\t"` (declared)
- Tags: `sql`
- Links: <https://dev.mysql.com/doc/refman/8.4/en/mysql-tips.html>,
  <https://docs.github.com/en/code-security/reference/secret-security/custom-patterns>,
  <https://github.com/google/re2/wiki/Syntax>

### postgres-explain

**PostgreSQL query plan** · level `info` · default · block

A PostgreSQL EXPLAIN or EXPLAIN ANALYZE plan, as psql or auto\_explain prints it: from the first node line with its "(cost=A..B rows=N width=W)" through the indented node and detail lines and the "Execution Time:" line. False positives are rare: the cost shape is specific.

- Keywords: `"(cost="`; and `" rows="`; and `" width="` (derived from the regex)
- Tags: `postgres`, `sql`
- Links: <https://www.postgresql.org/docs/current/using-explain.html>,
  <https://www.postgresql.org/docs/current/auto-explain.html>

### postgres-slow-statement

**PostgreSQL slow statement** · level `medium` · default · regex

A statement PostgreSQL logged because it ran longer than log\_min\_duration\_statement: "duration: N ms statement:" (or execute, parse, bind). A server that logs every statement's duration fills a log with these lines, and each is a finding.

- Keywords: `"duration: "`; and `" ms "`; and `" statement"` or `" execute"` or `" parse"` or `" bind"` (derived from the regex)
- Tags: `postgres`, `sql`, `performance`
- Links: <https://www.postgresql.org/docs/current/runtime-config-logging.html>,
  <https://raw.githubusercontent.com/postgres/postgres/master/src/backend/tcop/postgres.c>

### postgres-deadlock

**PostgreSQL deadlock** · level `high` · default · block

A deadlock PostgreSQL detected and broke: the "ERROR: deadlock detected" line, the DETAIL lines naming the processes and the locks each waits for, and the HINT, CONTEXT and STATEMENT lines that follow. False positives are rare.

- Keywords: `"error:"`; and `"deadlock detected"` (derived from the regex)
- Tags: `postgres`, `sql`, `locks`
- Links: <https://raw.githubusercontent.com/postgres/postgres/master/src/backend/storage/lmgr/deadlock.c>,
  <https://www.postgresql.org/docs/current/explicit-locking.html#LOCKING-DEADLOCKS>

### postgres-lock-wait

**PostgreSQL lock wait** · level `medium` · default · regex

A PostgreSQL process still waiting for a lock after deadlock\_timeout, logged with log\_lock\_waits: "process N still waiting for LOCK on OBJECT after T ms". False positives are rare.

- Keywords: `"process "`; and `" still waiting for "`; and `" on "`; and `" after "`; and `" ms"` (derived from the regex)
- Tags: `postgres`, `sql`, `locks`
- Links: <https://raw.githubusercontent.com/postgres/postgres/master/src/backend/storage/lmgr/proc.c>,
  <https://www.postgresql.org/docs/current/runtime-config-logging.html>

### mysql-slow-query

**MySQL slow query** · level `medium` · default · block

An entry of MySQL's slow query log: the "\# Time:" or "\# User@Host:" header, the "\# Query\_time:" line, "SET timestamp=N;" and the statement, through its closing ";".

- Keywords: `"# time: "` or `"# user@host: "` (derived from the regex)
- Tags: `mysql`, `sql`, `performance`
- Links: <https://dev.mysql.com/doc/refman/8.4/en/slow-query-log.html>,
  <https://raw.githubusercontent.com/mysql/mysql-server/trunk/sql/log.cc>

## http

HTTP requests answered with a server error.

### http-5xx

**HTTP server error** · level `medium` · default · regex

A request answered with a 5xx status: an access-log line in the common or combined format ('host ident user \[time\] "request" 5xx size'), a JSON log with "status" (or "status\_code", "statusCode") of 5xx, or a logfmt status=5xx. A log of many failed requests has one finding per request.

- Keywords: `"] \""` or `"\"status"` or `"status=5"` (derived from the regex)
- Tags: `http`, `access-log`
- Links: <https://httpd.apache.org/docs/2.4/logs.html>,
  <https://raw.githubusercontent.com/logstash-plugins/logstash-patterns-core/main/patterns/ecs-v1/httpd>

## severity

Lines written at an error or warning level.

### error-line

**Error line** · level `medium` · default · regex

A line that holds an upper-case error level word: FATAL, CRITICAL, CRIT, EMERG, ALERT, PANIC, SEVERE, ERROR or ERR, as a whole word. Lower-case "error" is common in ordinary messages ("0 errors") and is not found, so a log that writes its levels in lower case ("level=error") gives no findings here. The keyword gate compares without case, so every line holding "err" is tested.

- Keywords: `"fatal"` or `"critical"` or `"crit"` or `"emerg"` or `"alert"` or `"panic"` or `"severe"` or `"error"` or `"err"` (derived from the regex)
- Tags: `level`
- Links: <https://raw.githubusercontent.com/logstash-plugins/logstash-patterns-core/main/patterns/ecs-v1/grok-patterns>,
  <https://docs.lnav.org/en/latest/formats.html>

### warning-line

**Warning line** · level `low` · opt-in · regex

A line that holds an upper-case warning level word, WARN or WARNING, as a whole word. Opt-in: most logs hold many warnings. Lower-case "warning" is not found.

- Keywords: `"warn"` or `"warning"` (derived from the regex)
- Tags: `level`
- Links: <https://raw.githubusercontent.com/logstash-plugins/logstash-patterns-core/main/patterns/ecs-v1/grok-patterns>

## system

Resource and network failures such as a full disk, too many open files, timeouts, TLS and DNS errors.

### disk-full

**Disk full** · level `high` · default · regex

A write that failed because the file system is full, over quota or read-only: "No space left on device" (ENOSPC), "Disk quota exceeded" (EDQUOT), "Read-only file system" (EROFS).

- Keywords: `"no space left on device"` or `"disk quota exceeded"` or `"read-only file system"` or `"enospc"` (derived from the regex)
- Tags: `disk`, `errno`
- Links: <https://raw.githubusercontent.com/bminor/glibc/master/sysdeps/gnu/errlist.h>

### too-many-open-files

**Too many open files** · level `high` · default · regex

A process or the system out of file descriptors: "Too many open files" (EMFILE) or "Too many open files in system" (ENFILE).

- Keywords: `"too many open files"` or `"emfile"` or `"enfile"` (derived from the regex)
- Tags: `files`, `errno`
- Links: <https://raw.githubusercontent.com/bminor/glibc/master/sysdeps/gnu/errlist.h>

### out-of-memory

**Out of memory** · level `high` · default · regex

An allocation that failed: "Cannot allocate memory" (ENOMEM), "out of memory", Java's OutOfMemoryError, Node's "JavaScript heap out of memory" or Python's MemoryError. The kernel OOM killer's own lines are found by oom-killer as well.

- Keywords: `"cannot allocate memory"` or `"out of memory"` or `"outofmemoryerror"` or `"enomem"` or `"memoryerror"` (derived from the regex)
- Tags: `memory`, `errno`
- Links: <https://raw.githubusercontent.com/bminor/glibc/master/sysdeps/gnu/errlist.h>,
  <https://docs.oracle.com/en/java/javase/21/docs/api/java.base/java/lang/OutOfMemoryError.html>

### connection-failure

**Connection failure** · level `medium` · default · regex

A connection that could not be made or broke: "Connection refused" (ECONNREFUSED), "Connection reset by peer" (ECONNRESET), "No route to host" (EHOSTUNREACH), "Address already in use" (EADDRINUSE), "Broken pipe" (EPIPE). A client that retries often logs each try.

- Keywords: `"connection refused"` or `"connection reset by peer"` or `"no route to host"` or `"address already in use"` or `"broken pipe"` or `"econnrefused"` or `"econnreset"` or `"ehostunreach"` or `"eaddrinuse"` or `"epipe"` (derived from the regex)
- Tags: `network`, `errno`
- Links: <https://raw.githubusercontent.com/bminor/glibc/master/sysdeps/gnu/errlist.h>

### network-timeout

**Network timeout** · level `medium` · default · regex

A network operation that timed out: Go's "i/o timeout" and "context deadline exceeded", "Client.Timeout exceeded", "TLS handshake timeout", "Connection timed out" (ETIMEDOUT), "operation timed out", "read timed out", "request timed out". A context deadline is not always a network one.

- Keywords: `"i/o timeout"` or `"context deadline exceeded"` or `"client.timeout exceeded"` or `"tls handshake timeout"` or `"connection timed out"` or `"operation timed out"` or `"read time"` or `"request timed out"` or `"etimedout"` (derived from the regex)
- Tags: `network`, `timeout`
- Links: <https://raw.githubusercontent.com/golang/go/master/src/internal/poll/fd.go>,
  <https://raw.githubusercontent.com/golang/go/master/src/context/context.go>,
  <https://raw.githubusercontent.com/bminor/glibc/master/sysdeps/gnu/errlist.h>

### tls-failure

**TLS failure** · level `medium` · default · regex

A TLS handshake or certificate check that failed: Go's "http: TLS handshake error" and "x509: certificate ..." errors, TLS alerts such as "tls: bad certificate", OpenSSL's "SSL routines", "certificate verify failed", Java's SSLHandshakeException and "PKIX path building failed".

- Keywords: `"tls handshake error"` or `"x509: certificate"` or `"handshake failure"` or `"bad certificate"` or `"unknown certificate authority"` or `"protocol version not supported"` or `"internal error"` or `"first record does not look like a tls handshake"` or `"ssl routines"` or `"certificate verify failed"` or `"sslhandshakeexception"` or `"pkix path building failed"` (derived from the regex)
- Tags: `network`, `tls`
- Links: <https://raw.githubusercontent.com/golang/go/master/src/crypto/x509/verify.go>,
  <https://raw.githubusercontent.com/golang/go/master/src/crypto/tls/alert.go>,
  <https://raw.githubusercontent.com/golang/go/master/src/net/http/server.go>

### dns-failure

**DNS failure** · level `medium` · default · regex

A name that could not be resolved: Go's "no such host" and "server misbehaving", "Temporary failure in name resolution", "Name or service not known", "Could not resolve host", Node's "getaddrinfo ENOTFOUND", Java's UnknownHostException, or the DNS answers NXDOMAIN and SERVFAIL. A DNS server's query log holds an NXDOMAIN for every unknown name asked, and each is a finding.

- Keywords: `"no such host"` or `"nxdomain"` or `"servfail"` or `"temporary failure in name resolution"` or `"name or service not known"` or `"server misbehaving"` or `"could not resolve host"` or `"unknownhostexception"` or `"getaddrinfo enotfound"` or `"getaddrinfo eai_again"` (derived from the regex)
- Tags: `network`, `dns`
- Links: <https://raw.githubusercontent.com/golang/go/master/src/net/net.go>,
  <https://datatracker.ietf.org/doc/html/rfc1035#section-4.1.1>

## lifecycle

Signals, exit statuses and service state changes.

### signal-received

**Signal received** · level `low` · default · regex

A process reporting a signal it received: "received", "got", "caught" or "handling" then "signal" and a name, or a SIG name (SIGTERM, SIGINT, SIGHUP, SIGQUIT, SIGKILL, SIGUSR1, SIGUSR2, SIGABRT, SIGPIPE, SIGALRM), or the names Go prints ("terminated", "interrupt", "hangup"); or nginx's "signal N (SIGNAME) received". The usual sign of a stop, a reload or a restart.

- Keywords: `"received signal"` or `"received sigterm"` or `"received sigint"` or `"received sighup"` or `"received sigquit"` or `"received sigkill"` or `"received sigusr"` or `"received sigabrt"` or `"received sigpipe"` or `"received sigalrm"` or `"got signal"` or `"got sigterm"` or `"got sigint"` or `"got sighup"` or `"got sigquit"` or `"got sigkill"` or `"got sigusr"` or `"got sigabrt"` or `"got sigpipe"` or `"got sigalrm"` or `"caught signal"` or `"caught sigterm"` or `"caught sigint"` or `"caught sighup"` or `"caught sigquit"` or `"caught sigkill"` or `"caught sigusr"` or `"caught sigabrt"` or `"caught sigpipe"` or `"caught sigalrm"` or `"handling signal"` or `"handling sigterm"` or `"handling sigint"` or `"handling sighup"` or `"handling sigquit"` or `"handling sigkill"` or `"handling sigusr"` or `"handling sigabrt"` or `"handling sigpipe"` or `"handling sigalrm"` or `") received"` (derived from the regex)
- Tags: `signal`
- Links: <https://man7.org/linux/man-pages/man7/signal.7.html>

### exit-status

**Non-zero exit status** · level `low` · default · regex

A process that ended with a non-zero exit status or code: "exit status 2", "exited with code 1", "exit code=137", or systemd's "code=exited, status=1/FAILURE". A zero status is not found.

- Keywords: `"exit status"` or `"exit code"` or `"exited status"` or `"exited code"` or `"exited with status "` or `"exited with code "` or `"code=exited, status="` (derived from the regex)
- Tags: `exit`
- Links: <https://man7.org/linux/man-pages/man3/exit.3.html>,
  <https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#Process%20Exit%20Codes>

### systemd-unit-change

**systemd unit change** · level `info` · default · regex

systemd (PID 1) starting, stopping, reloading or failing a unit: "systemd\[1\]:" then a unit name ending in .service, .socket, .timer, .mount and the like, and Started, Stopped, Reloaded, "Failed with result", "Main process exited" or "Scheduled restart job". A host that starts a session or a timer every minute logs one finding each time.

- Keywords: `"systemd[1]: "`; and `"started"` or `"stopped"` or `"starting"` or `"stopping"` or `"reloaded"` or `"reloading"` or `"failed to start"` or `"deactivated successfully"` or `"scheduled restart job"` or `".service: failed with result"` or `".service: main process exited"` or `".socket: failed with result"` or `".socket: main process exited"` or `".timer: failed with result"` or `".timer: main process exited"` or `".mount: failed with result"` or `".mount: main process exited"` or `".scope: failed with result"` or `".scope: main process exited"` (derived from the regex)
- Tags: `systemd`, `linux`
- Links: <https://www.freedesktop.org/software/systemd/man/latest/systemd.html>

## structure

The shape of the text, such as runs of blank or repeated lines, long lines, JSON blocks and binary bytes.

### blank-run

**Run of blank lines** · level `info` · default · run of blank lines, at least 3

Three or more consecutive lines that are empty or hold only spaces and tabs (a line ending in "\\r\\n" counts as empty). A log written with blank lines between its records has one finding per gap of three or more.

- Keywords: none: tried on every line
- Tags: `blank`
- Links: <https://raw.githubusercontent.com/BurntSushi/ripgrep/master/GUIDE.md>

### long-line

**Long line** · level `low` · default · measure `long-line` (window 100, factor 10, min_bytes 1024)

A line much longer than the lines around it: at least ten times the median length of the 100 lines on each side, and at least 1,024 bytes. A file whose lines are all long has none; a log that writes large payloads now and then has one finding per payload.

- Keywords: none: tried on every line
- Tags: `length`
- Links: <https://www.itl.nist.gov/div898/handbook/eda/section3/eda35h.htm>

### repeated-lines

**Repeated lines** · level `low` · default · run of equal lines, at least 5

Five or more consecutive lines equal byte for byte: a loop that logs the same message, often a retry storm. Lines that differ only by a timestamp or a counter are not equal and are not found.

- Keywords: none: tried on every line
- Tags: `repeat`
- Links: <https://raw.githubusercontent.com/systemd/systemd/main/src/journal/journald-manager.c>

### repeat-suppressed

**Repeated messages suppressed** · level `low` · default · regex

A logger saying it dropped repeats of a message: journald's "Suppressed N messages from UNIT", rsyslog's "message repeated N times", the older "last message repeated N times", the kernel's "N callbacks suppressed". A sign of a storm the log does not show in full.

- Keywords: `" messages from"` or `"message repeated "` or `"last message repeated "` or `"last line repeated "` or `" callbacks suppressed"` (derived from the regex)
- Tags: `repeat`, `syslog`
- Links: <https://raw.githubusercontent.com/systemd/systemd/main/src/journal/journald-manager.c>,
  <https://docs.rsyslog.com/doc//configuration/action/rsconf1_repeatedmsgreduction.html>

### json-block

**JSON block** · level `info` · default · measure `json-block` (max_lines 200)

A JSON object or array printed over several lines: a line that is only "{" or "\[" through the matching closer at the same indent, counted with strings taken into account (braces inside a string do not count). A JSON document on one line is not found.

- Keywords: none: tried on every line
- Tags: `json`
- Links: <https://datatracker.ietf.org/doc/html/rfc8259>

### binary-bytes

**Binary bytes** · level `medium` · default · measure `binary`

A run of lines that hold a NUL byte or bytes that are not UTF-8: a binary dump or a corrupted write inside a text log. ripgrep stops reading a file at a NUL byte unless told otherwise, so this also shows where other tools would stop. A file with a NUL byte in its first 8 KiB is not text and is not scanned at all.

- Keywords: none: tried on every line
- Tags: `binary`
- Links: <https://raw.githubusercontent.com/BurntSushi/ripgrep/master/GUIDE.md>

### base64-blob

**Base64 blob** · level `low` · default · measure `base64-run` (min_length 100)

A run of at least 100 base64 characters (A-Z a-z 0-9 + /, with "=" padding) inside a line: an encoded attachment, image, key or payload. Long hex strings are base64 characters too and are found.

- Keywords: none: tried on every line
- Tags: `base64`
- Links: <https://datatracker.ietf.org/doc/html/rfc4648#section-4>

### hex-dump

**Hex dump** · level `low` · default · run, at least 2 lines

Two or more consecutive lines of a hex dump: an offset, then eight to sixteen two-digit hex bytes (hexdump -C, od, a debugger), or the xxd form with groups of four digits. A table of small numbers can look the same.

- Keywords: none: tried on every line
- Tags: `hex`, `binary`
- Links: <https://man7.org/linux/man-pages/man1/hexdump.1.html>

### unprefixed-run

**Lines without a timestamp** · level `info` · default · measure `unprefixed` (min 5, min_share 0.5)

Five or more consecutive lines without a timestamp at their start in a log whose lines mostly have one: a multi-line message, a dump or a stack trace written without the log's prefix. Not looked for when fewer than half of the lines are stamped.

- Keywords: none: tried on every line
- Tags: `timestamps`
- Links: <https://docs.lnav.org/en/latest/formats.html>

## time

The timing of the log, such as bursts of lines, silences and timestamps that go backwards.

### log-burst

**Burst of log lines** · level `medium` · default · measure `density` (bucket auto, factor 5, z 3.5, min_count 100)

A burst of lines: consecutive time buckets that each hold at least five times the median number of lines per bucket, a modified z-score over 3.5 and at least 100 lines. The median and the MAD of the z-score are taken over the buckets that hold a line, each the lower middle value of an even count. The bucket size is chosen for about 1,000 buckets over the file's time span. The counts per bucket are kept with the findings, for a density chart.

- Keywords: none: tried on every line
- Tags: `density`
- Links: <https://www.itl.nist.gov/div898/handbook/eda/section3/eda35h.htm>,
  <https://www.elastic.co/guide/en/machine-learning/current/ml-count-functions.html>

### time-gap

**Silence in the log** · level `low` · default · measure `time-gap` (min_gap 1m, factor 50)

A silence: at least a minute, and at least 50 times the median interval between stamped lines (the lower middle value of an even count), with no stamped line. The finding is the line before the silence and the first stamped line after it. A log that only writes when something happens has many.

- Keywords: none: tried on every line
- Tags: `gap`
- Links: <https://www.elastic.co/guide/en/machine-learning/current/ml-count-functions.html>

### time-backwards

**Timestamp going backwards** · level `low` · default · measure `time-backwards` (tolerance 1s)

A line stamped more than a second earlier than the stamped line before it: a clock step, two writers with different clocks, or records merged out of order. Small steps back from concurrent writers are within the tolerance.

- Keywords: none: tried on every line
- Tags: `clock`
- Links: <https://docs.lnav.org/en/latest/formats.html>
