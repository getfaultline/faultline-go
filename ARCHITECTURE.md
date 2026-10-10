# Architecture decision log: `faultline-go`

## Layers

```
faultline (root)            public API + composition root: Init, Capture*, Recover, Scope,
                            NewContext, WithStack; the only place that wires adapters
internal/
  domain/                   pure: event contract types, Scope, options validation, limits,
                            scrubbing, JSON normalization, sanitization, dedupe key
  application/              Client (builds/filters/enriches events), Hub (client + scopes)
    ports.go                Transport, StackParser, SourceContextReader, Clock, IDGenerator,
                            Random, Logger, RuntimeInfo
  infrastructure/
    transport/              batched HTTP transport (gzip, retries, 429, queue)
    stack/                  runtime.Callers frames, error-carried stacks, inApp rules
    source/                 source-context reader with an LRU cache
    sysinfo/                runtime info, clock, uuid v4, math/rand, stderr logger
  contracttest/             test-only: JSON Schema of the contract, validator, mock server
faultlinehttp/              net/http middleware (public API only)
faultlineslog/              slog.Handler (public API only)
faultlinegin/               Gin middleware, separate module (public API only)
examples/nethttp/           runnable demo
```

Dependency rule: `domain ← application ← infrastructure ← root ← integrations`.

---

### ADR-001: Enforce boundaries with a test that reads imports

- **Status:** accepted
- **Decision:** `architecture_test.go` walks the module with `go/build`, reads each package's
  non-test imports and checks them against per-layer rules:
  - `domain` imports nothing from the module and no I/O packages (`os`, `net`, `net/http`,
    `runtime`, `log`, `io/fs`, `syscall`, `path/filepath`, `crypto/rand`).
  - `application` may import only `domain`, with the same standard-library ban.
  - `infrastructure` may import `domain` and `application`.
  - the root package may import everything internal.
  - integrations may import only the root package, never `internal/...`.
- **Why:** The Go toolchain has no boundary linter in the standard distribution, and the
  `internal/` rule alone does not stop inner layers from importing outer ones. `go/build`
  needs no dependency and no subprocess.
- **Verification:** `TestArchitectureRulesCatchViolations` feeds known-bad import lists to the
  same checker, so the rule test cannot pass vacuously.

### ADR-002: Standard library only; nested module for Gin

- **Decision:** The core module has no `require` lines. `faultlinegin` is its own module
  (`github.com/getfaultline/faultline-go/faultlinegin`) that requires Gin and the core
  module, with `replace ../` for local development.
- **Why:** Users of `net/http`, Echo, Chi and others should not download Gin and its
  transitive dependencies. A nested module keeps `go.sum` of the core empty.
- **Consequence:** The Gin module is tagged separately (`faultlinegin/v0.1.0`). Its tests
  import `internal/contracttest` from the core; Go allows this because the import path of
  the Gin module is under the core module's path.

### ADR-003: Options validation never panics; `Init` returns an error

- **Decision:** `domain.ResolveOptions` validates by hand. Invalid optional values fall back
  to their default with a warning; a missing `APIKey` (while enabled) or an unusable
  `Endpoint` are fatal and leave the SDK disabled. `Init` returns one error wrapping
  `ErrInvalidOptions` that lists both kinds, so callers can log it; `IsEnabled` tells them
  whether the SDK runs.
- **Zero values:** Go cannot distinguish "unset" from zero, so `SampleRate: 0` means 1 (use
  `Enabled` to stop sending), `MaxBreadcrumbs: 0` means 50 (negative disables), and
  `Enabled` is a `*bool` (`faultline.Bool(false)`).
- **IgnoreErrors:** The Node SDK accepts strings (substring) and RegExps. Go has one field of
  `[]string` compiled as regular expressions; a plain string without metacharacters behaves
  like a substring, and an entry that does not compile is matched literally with a warning.

### ADR-004: Scopes: context-carried for concurrency, a stack for `WithScope`

- **Problem:** Node uses AsyncLocalStorage. Go has no goroutine-local storage.
- **Decision:**
  - `NewContext(ctx)` stores a fork of the parent scope in the context;
    `ScopeFromContext`, `CaptureExceptionContext`, `CaptureMessageContext`,
    `AddBreadcrumbContext` and `RecoverWithContext` use it. The middleware create one per
    request, which gives the same isolation guarantee as the Node `requestHandler`.
  - `WithScope(f)` pushes a fork onto a mutex-protected stack in the Hub and pops it when `f`
    returns or panics, so `faultline.CaptureException` inside `f` sees it (the Node usage
    pattern keeps working). Pops remove the specific scope, so interleaved goroutines cannot
    pop each other's scopes. Package-level captures from other goroutines can still observe
    a pushed fork while it is active; this is documented and the scope's own `Capture*`
    methods avoid it.
- **Scope:** `domain.Scope` is guarded by an `RWMutex`; `Data()` returns a deep-enough
  snapshot that later changes do not affect an event. The public `*Scope` wraps it and every
  method is nil-safe.

### ADR-005: Event processing is synchronous up to the transport

- **Decision:** `Capture*` builds the exception, applies sampling, the scope, `IgnoreErrors`,
  dedupe, source context, sanitization, scrubbing and `BeforeSend` in the calling goroutine,
  then enqueues the serialized event. Only delivery is asynchronous.
- **Why:** Simpler than Node's split pipeline, deterministic in tests, and `Flush` only has
  to drain the transport. The costly step (reading source files) is cached by an LRU, so
  each file is read once.
- **Consequence:** Events reach the server in capture order per goroutine.

### ADR-006: Exception chain

- **Traversal:** depth first over `Unwrap() error` and `Unwrap() []error`, at most 5
  exceptions in total (root plus 4 causes, same interpretation as Node ADR-007), cycles
  broken with a `seen` set (only for comparable dynamic types; comparing a non-comparable
  error would panic).
- **Type:** `fmt.Sprintf("%T", err)`, the concrete type name required by CONTRACTS section 7.
- **Stacks:** the root uses the error's own stack when it carries one, otherwise the
  `runtime.Callers` stack of the capture site. Causes use their own stack or none.
- **Stack-only wrappers:** a wrapper with its own stack and the same message as its single
  child (`faultline.WithStack`, pkg/errors `withStack`) is folded into the child, which
  inherits the stack when it has none. Otherwise every chain would start with a duplicate
  `*faultline.stackError` / `*errors.withStack` entry.
- **Error-carried stacks:** `Callers() []uintptr` (go-errors, and our `WithStack`) and, via
  reflection, any `StackTrace()` method returning a slice of `uintptr`-kinded values
  (pkg/errors `errors.StackTrace`). This avoids depending on those packages.

### ADR-007: Panics

- **`Recover` / `RecoverWithContext`** call `recover()` themselves (they must be deferred
  directly), capture at level `fatal`, then by default flush for up to 2s and re-panic with
  the original value, so the program behaves as without the SDK (the Node process
  integration preserves the default exit the same way). `SwallowPanics` opts out. Without an
  initialized SDK they always re-panic.
- **`CapturePanic`** is for middleware that must call `recover()` itself (to pass
  `http.ErrAbortHandler` through, or to decide whether to re-panic).
- **Frames:** the stack captured inside the deferred call contains the recovery machinery.
  Everything newer than `runtime.gopanic` is dropped, and so are the runtime helpers directly
  below it (`runtime.panicmem`, `runtime.sigpanic`, ...), so the newest frame is the code that
  panicked or faulted.
- **Values:** errors (including `runtime.Error`) keep their concrete type; other values become
  type `panic` with `fmt.Sprint(v)` as the value (guarded against panicking `String` methods).

### ADR-008: inApp detection

- **Rule (CONTRACTS section 7):** not in GOROOT and not under `/pkg/mod/`.
- **GOROOT:** derived from the file path of a `runtime` frame at startup (`.../src/runtime/x.go`
  gives `.../src/`), which is the GOROOT the binary was built with, falling back to
  `runtime.GOROOT()`.
- **`-trimpath` builds:** file names are relative; the parser detects this from the runtime
  frame and then treats paths whose first element has no dot (standard library) or that
  contain `@v` (module dependencies) as not in-app.

### ADR-009: Transport

- **Shape:** one goroutine per client; `Send` serializes (trimming breadcrumbs, then source
  context, then extra to fit 1 MB) and appends under a mutex, dropping the oldest event past
  100. The goroutine wakes on a 2s ticker, when 20 events are queued, or when `Flush` asks.
- **Batches:** at most 50 events and 1 MB uncompressed (`{"events":[...]}`), gzip.
- **Retries:** network errors, timeouts (5s, via context), `5xx` and `408` are retried inline
  up to 3 times with equal jitter (`min(30s, 1s*2^n)/2 + rand*min(30s, 1s*2^n)/2`). A `429`
  sets a pause until `Retry-After` (seconds or HTTP date, default 10s, capped at 10 min) and
  puts the batch back at the front of the queue; an event is dropped after 3 rate-limited
  attempts. `401`, `413` and other `4xx` drop the batch.
- **Flush:** polls (5 ms) until the queue is empty and nothing is in flight, returning
  `false` at the deadline or immediately when a rate-limit pause outlasts it.
- **Close:** flushes, marks the transport closed, drops the remainder and stops the goroutine.
- **No `unref`:** Go programs exit when `main` returns regardless of background goroutines,
  so the transport never keeps a process alive; users call `Close` to deliver pending events.

### ADR-010: Contract conformance without dependencies

- **Decision:** `internal/contracttest/event.schema.json` mirrors CONTRACTS sections 2 and 7
  as a JSON Schema (draft 2020-12 subset). A ~200-line validator implements only the
  keywords the schema uses. The cause-depth limit is checked in Go because JSON Schema cannot
  express it without unrolling. The mock ingest server validates every request body in
  the root, net/http, slog and Gin tests.
- **Why:** The Node SDK uses zod for the same purpose; Go has no schema validator in the
  standard library and the core must stay dependency-free. The package is imported only from
  `_test.go` files, so it is never linked into the SDK.
- **Server check:** the schema also encodes the server's own extra limits (request header and
  query keys ≤ 256 chars), which `SanitizeEvent` enforces.

### ADR-011: Debug logging bypasses `log` and `slog` defaults

- **Decision:** The debug logger writes to `os.Stderr` through its own `*log.Logger`.
- **Why:** `slog.SetDefault` redirects the `log` package's default logger into slog. If the
  SDK logged through either default and the app used `faultlineslog`, SDK logs would become
  breadcrumbs (or events) and could loop.

### ADR-012: Toolchain

- **Language level:** `go 1.21` in both `go.mod` files; the code avoids range-over-int,
  range-over-func and `math/rand/v2`. CI runs vet and `go test -race` on Go 1.21, 1.22 and
  1.23, and gofmt plus staticcheck 2025.1.1 on Go 1.24.
- **Local note:** on macOS 26, binaries linked by Go 1.21's internal linker fail to load
  (`missing LC_UUID`); run Go 1.21 tests there with `-ldflags=-linkmode=external`. Linux CI is
  unaffected. Released staticcheck builds cannot read Go 1.27 export data yet, so locally it
  runs with `GOTOOLCHAIN=go1.24.6`.

### Suggestions (not implemented, outside spec)

- A `Transport` option for custom delivery (serverless, tests).
- Offline persistence of the queue across restarts.
- Integrations for Echo and Chi, and an `http.RoundTripper` that records outgoing requests as
  breadcrumbs (the Node SDK's http integration).
