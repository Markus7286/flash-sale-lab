.PHONY: up down logs test psql redis-cli

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f

# The tests skip themselves rather than fail when Postgres or Redis is unreachable.
test:
	go test ./... -count=1

psql:
	docker compose exec postgres psql -U flashsale -d flashsale

redis-cli:
	docker compose exec redis redis-cli
