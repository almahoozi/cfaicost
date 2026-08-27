.PHONY: all build clean run test install

all: clean test

build:
	go build -o ./bin/cfaicost .

clean:
	rm -rf ./bin

install:
	go install .

run:
	go run .

test:
	go test -v ./...
