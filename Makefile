COMPOSE ?= $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo docker-compose)

.PHONY: poc test bench real down

poc:
	$(COMPOSE) run --rm --build poc $(ARGS)

test:
	$(COMPOSE) run --rm --build test

bench:
	$(COMPOSE) run --rm --build test go test -run '^$$' -bench . -benchmem ./...

# make real CONFIG_FILE=/path/to/config.json  -> mounts the file read-only, never copies it into the image
real:
	@test -n "$(CONFIG_FILE)" || (echo "usage: make real CONFIG_FILE=/abs/path/config.json" && exit 1)
	$(COMPOSE) run --rm --build -v "$(CONFIG_FILE):/data/config.json:ro" poc -config-file /data/config.json $(ARGS)

down:
	$(COMPOSE) down -v --remove-orphans
