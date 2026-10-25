.PHONY: build test boot-image

build:
	bash scripts/check.sh build

test:
	bash scripts/check.sh test

boot-image:
	bash scripts/build-boot.sh
