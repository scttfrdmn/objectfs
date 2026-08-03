module github.com/scttfrdmn/objectfs

go 1.26.0

// The `go` line above is the language minimum. This is what actually builds, and the two must be
// separate directives or CI pins the patch release too.
//
// `actions/setup-go` with `go-version-file: go.mod` reads the `go` line as an exact version spec,
// so `go 1.26.0` alone installed go1.26.0 — five patch releases behind — for every job in every
// workflow, including the ones that produce release binaries. govulncheck reported **19 standard
// library advisories** against those builds, among them crypto/tls fixed in 1.26.5, crypto/x509 in
// 1.26.4, and net/http in 1.26.3. None was a defect in this code; all were shipped by it.
//
// `toolchain` is therefore the line that decides what CI compiles with. Bump it when a patch
// release carries a fix worth having, which for a filesystem handling other people's data is most
// of them.
//
// How it gets honoured changed with setup-go v7, and the mechanism is worth knowing because the
// two paths fail differently. v5 installed the `go` line (1.26.0) and Go's own toolchain switching
// then downloaded go1.26.5 on the first build — correct result, but reached at build time, over the
// network, per job, and visibly: the `tar: ... gotoolchain_local.txt: Cannot open: File exists`
// noise in every v5 job log is that download racing the module cache restore. v7 parses `toolchain`
// directly and exports `GOTOOLCHAIN=local`, so 1.26.5 is installed up front and nothing switches.
//
// The consequence to remember: under `GOTOOLCHAIN=local` there is no longer a safety net. If this
// line names a version setup-go cannot install, the build fails outright rather than quietly
// switching to something that works.
toolchain go1.26.5

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/aws/aws-sdk-go-v2 v1.43.3
	github.com/aws/aws-sdk-go-v2/config v1.32.33
	github.com/aws/aws-sdk-go-v2/credentials v1.19.32
	github.com/aws/aws-sdk-go-v2/service/s3 v1.106.3
	github.com/aws/smithy-go v1.27.6
	github.com/cespare/xxhash/v2 v2.3.0
	github.com/hanwen/go-fuse/v2 v2.11.0
	github.com/klauspost/compress v1.19.1
	github.com/pierrec/lz4/v4 v4.1.27
	github.com/prometheus/client_golang v1.22.0
	github.com/prometheus/client_model v0.6.2
	github.com/redis/go-redis/v9 v9.21.0
	github.com/scttfrdmn/cargoship v0.16.0
	github.com/scttfrdmn/substrate v0.85.0
	github.com/stretchr/testify v1.11.1
	golang.org/x/sync v0.22.0
	golang.org/x/sys v0.46.0
	gopkg.in/yaml.v2 v2.4.0
)

require (
	github.com/andybalholm/brotli v1.2.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.16 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.18.33 // indirect
	github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.17.82 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.4.34 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.7.34 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.4.35 // indirect
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.45.3 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.15 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.9.27 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.13.34 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.19.35 // indirect
	github.com/aws/aws-sdk-go-v2/service/pricing v1.34.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.5.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.33.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.38.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.45.2 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-chi/chi/v5 v5.3.1 // indirect
	github.com/go-chi/cors v1.2.2 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/common v0.65.0 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/spf13/viper v1.21.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.40.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.44.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.44.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.82.1 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.55.0 // indirect
)
