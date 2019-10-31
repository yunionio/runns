SOURCES := $(shell find . 2>&1 | grep -E '.*\.(c|h|go)$$')

runns: $(SOURCES)
	go build -o runns . -static

all: runns

clean:
	rm -f runns

.PHONY: all clean
