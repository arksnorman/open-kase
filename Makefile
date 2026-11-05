# Root delegating Makefile. The UI (ui/) has its own tooling.
.PHONY: all fmt vet test test-integration build clean

all:
	$(MAKE) -C api all

fmt vet test test-integration build clean:
	$(MAKE) -C api $@
