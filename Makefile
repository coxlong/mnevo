BINARY  ?= mnevo
VERSION ?= dev
LDFLAGS := -s -w -X github.com/coxlong/mnevo/internal/cli.Version=$(VERSION)
OUT     ?= dist/$(BINARY)

# Tool overrides for regenerating committed frontend assets.
TEMPL        ?= templ
TAILWIND     ?= tailwindcss
TAILWIND_SRC ?= internal/web/assets/input.css
TAILWIND_OUT ?= internal/web/assets/app.css

.PHONY: all build install test race vet check clean diagram frontend css generate

all: check build

# generate compiles .templ components into Go code.
generate:
	$(TEMPL) generate

# css rebuilds the Tailwind stylesheet from input.css.
css:
	$(TAILWIND) -i $(TAILWIND_SRC) -o $(TAILWIND_OUT) --minify

frontend: generate css

build:
	@mkdir -p $(dir $(OUT))
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) .

install: build
	@mkdir -p $(HOME)/.local/bin
	install -m 0755 $(OUT) $(HOME)/.local/bin/$(BINARY)

test:
	CGO_ENABLED=0 go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: vet test race

diagram:
	dot -Tsvg docs/runtime-architecture.dot -o docs/runtime-architecture.svg

clean:
	rm -rf dist
