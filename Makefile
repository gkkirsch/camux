.PHONY: build test install clean

build:
	go build -o camux .

test:
	go test ./... -v -count=1

install: build
	install -m 0755 camux $(HOME)/.local/bin/camux

clean:
	rm -f camux
