ENV_FILE := ../.env

up:
	docker compose --env-file $(ENV_FILE) up -d

down:
	docker compose --env-file $(ENV_FILE) down

logs:
	docker compose --env-file $(ENV_FILE) logs -f

restart:
	docker compose --env-file $(ENV_FILE) restart

build:
	docker compose --env-file $(ENV_FILE) up -d --build