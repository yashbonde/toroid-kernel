# Repo-root targets.

BINARY := examples/cli/cli

.PHONY: cli

# cli — build the smallest CLI binary from examples/cli and report its size
# plus the delta from the previously built binary (the current ./cli if present).
cli:
	@prev=0; [ -f $(BINARY) ] && prev=$$(stat -f%z "$(BINARY)" 2>/dev/null || stat -c%s "$(BINARY)" 2>/dev/null); \
	go build -ldflags "-s -w" -trimpath -o $(BINARY) ./examples/cli; \
	new=$$(stat -f%z "$(BINARY)" 2>/dev/null || stat -c%s "$(BINARY)" 2>/dev/null); \
	hr() { n=$$1; if [ $$n -ge 1048576 ]; then echo "$$((n / 1048576)).$$(( (n % 1048576) / 104857 ))MiB"; elif [ $$n -ge 1024 ]; then echo "$$((n / 1024))KiB"; else echo "$${n}B"; fi; }; \
	echo "new cli: $$(hr $$new) ($$new bytes)"; \
	if [ $$prev -gt 0 ]; then \
		delta=$$((new - prev)); \
		if [ $$delta -ge 0 ]; then \
			echo "delta: +$$(hr $$delta) (+$$delta bytes) vs previous $(BINARY)"; \
		else \
			echo "delta: -$$(hr $$((0 - delta))) ($$delta bytes) vs previous $(BINARY)"; \
		fi; \
	else \
		echo "delta: none (no previous $(BINARY) to compare)"; \
	fi
