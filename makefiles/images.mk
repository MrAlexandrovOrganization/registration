# CI builds/publishes once; VM activates immutable digests (Python 3 + Docker required).
export IMAGE_COMPOSE_FILE := compose/docker-compose.yml
export IMAGE_ALIASES := migrate=backend
IMAGE_COMPOSE ?= $(DOCKER_COMPOSE)

.PHONY: image-build image-publish image-deploy image-activate
image-build:
	python3 scripts/images.py build
image-publish:
	python3 scripts/images.py publish
image-deploy:
	python3 scripts/images.py deploy
image-activate:
	$(IMAGE_COMPOSE) stop backend
	$(IMAGE_COMPOSE) up -d --no-build --pull never --wait --wait-timeout 180 postgres
	$(IMAGE_COMPOSE) run --rm --no-deps --pull never migrate
	$(IMAGE_COMPOSE) up -d --no-build --pull never --wait --wait-timeout 180 backend

.PHONY: image-check
image-check:
	python3 -B scripts/test_images.py

.PHONY: image-compose
export IMAGE_ARGS
image-compose:
	python3 scripts/images.py compose
