.PHONY: install build test lint e2e dev run preview clean

install:
	bun install --frozen-lockfile

build: install
	bun run build

test: install
	bun run test

lint: install
	bun run typecheck

e2e: build
	bun run test:e2e

dev: install
	bun run dev

preview: build
	bun run preview

run: preview

clean:
	rm -rf node_modules dist core/dist core/tsconfig.build.tsbuildinfo
