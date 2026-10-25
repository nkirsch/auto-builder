include app.defaults.env app.env
include board.mk

.PHONY: all build view clean

all: build

# Site
build:
	hugo --minify

view:
	hugo server --bind=0.0.0.0 --baseURL http://localhost:1313/

clean:
	rm -rf public resources
