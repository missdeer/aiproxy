.PHONY: all clean

# Detect OS and architecture
UNAME_S := $(shell uname -s 2>/dev/null || echo Windows)

ifeq ($(UNAME_S),Darwin)
    EXT :=
    IS_MACOS := 1
else ifeq ($(OS),Windows_NT)
    EXT := .exe
    IS_MACOS :=
else
    EXT :=
    IS_MACOS :=
endif

# Build flags
LDFLAGS := -ldflags="-s -w"

# Output directory
BINDIR := bin

# Targets
MAIN := $(BINDIR)/aiproxy$(EXT)

# Source files
MAIN_SRCS := $(wildcard *.go) $(wildcard */*.go) go.mod go.sum

all: $(MAIN) 

ifdef IS_MACOS
# macOS: Build FAT binary (Universal Binary) for arm64 and amd64
$(MAIN): $(MAIN_SRCS)
	@mkdir -p $(BINDIR)
	GOARCH=arm64 go build $(LDFLAGS) -o $@_arm64 .
	GOARCH=amd64 go build $(LDFLAGS) -o $@_amd64 .
	lipo -create -output $@ $@_arm64 $@_amd64
	@rm -f $@_arm64 $@_amd64
else
# Windows/Linux: Build single architecture
$(MAIN): $(MAIN_SRCS)
	@mkdir -p $(BINDIR)
	go build $(LDFLAGS) -o $@ .
endif

clean:
	rm -rf $(BINDIR)
