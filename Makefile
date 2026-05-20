BINARY=watchdog
INSTALL_DIR=$(HOME)/.local/bin

.PHONY: build install clean

build:
	go build -o $(BINARY) ./cmd/watchdog

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BINARY) $(INSTALL_DIR)/$(BINARY)
	# Re-sign: macOS AMFI invalidates Go's ad-hoc linker signature on `cp`,
	# which produces SIGKILL on first run. ditto would preserve it too.
	codesign --force --sign - $(INSTALL_DIR)/$(BINARY)
	@echo "Installed to $(INSTALL_DIR)/$(BINARY)"

clean:
	rm -f $(BINARY)
