.PHONY: web web-clean test lint

# web builds the browser GUI into web/dist, which is COMMITTED.
#
# Committing the build output is what keeps node out of everyone else's way:
# `go install github.com/richardwooding/forge/cmd/forge@latest` has to work
# with nothing but a Go toolchain, and `go build ./...` stays the only build a
# contributor needs. The cost is that dist can drift from src, which is what
# the diff check in CI is for.
web:
	cd web && npm ci && npm run build
	# vite empties outDir, which takes .gitkeep with it. That file is what
	# keeps `//go:embed all:dist` compiling in a tree where the GUI has never
	# been built -- without it, `go build ./...` fails for everyone the moment
	# dist is empty.
	touch web/dist/.gitkeep

web-clean:
	rm -rf web/dist && mkdir -p web/dist && touch web/dist/.gitkeep

test:
	go test ./...

lint:
	go vet -stdmethods=false ./...
	golangci-lint run ./...
