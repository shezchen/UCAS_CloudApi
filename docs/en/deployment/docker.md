# Docker Deployment

## Overview

This guide covers deploying AxonHub using Docker and Docker Compose. Docker provides an isolated, reproducible environment that simplifies deployment and scaling.

## Quick Start

### 1. Clone the Repository

```bash
git clone https://github.com/looplj/axonhub.git
cd axonhub
```

### 2. Configure Environment

Set the database password. `docker-compose.yml` has no default for it and
refuses to start while it is unset:

```bash
cp .env.example .env
sed -i "s/^DB_PASSWORD=.*/DB_PASSWORD=$(openssl rand -hex 24)/" .env
```

Copy the example configuration file:

```bash
cp config.example.yml config.yml
```

Edit `config.yml` with your settings:

```yaml
# config.yml
server:
  port: 8090
  name: "AxonHub"

db:
  dialect: "sqlite3"
  dsn: "file:axonhub.db?cache=shared&_fk=1&_pragma=journal_mode(WAL)"

log:
  level: "info"
  encoding: "json"
```

### 3. Start Services

```bash
docker-compose up -d
```

### 4. Verify Deployment

Check service status:

```bash
docker-compose ps
```

Access the application:
- Web interface: http://localhost:8090
- Default admin: admin@example.com / admin123

## Upgrading an Existing Deployment

`docker-compose.yml` used to fall back to a hard-coded `axonhub_password` when
`DB_PASSWORD` was unset. That fallback is gone, so a stack that relied on it
stops with `DB_PASSWORD is required` until a value is supplied — and supplying
a *different* one is not enough on its own.

PostgreSQL reads `POSTGRES_PASSWORD` only while it initialises a new data
directory. An existing `postgres_data` volume keeps the credential it was
created with, so a new `DB_PASSWORD` leaves AxonHub failing to connect with
`password authentication failed for user "axonhub"`. Choose one of the two:

**Keep the credential the volume already has.** Nothing changes inside
PostgreSQL; the old default just becomes explicit:

```bash
cp .env.example .env
sed -i 's/^DB_PASSWORD=.*/DB_PASSWORD=axonhub_password/' .env
docker compose up -d
```

**Rotate to a new one.** Change it inside PostgreSQL first, so both sides end
up matching:

```bash
NEW_PASSWORD="$(openssl rand -hex 24)"

docker compose exec postgres \
  psql -U axonhub -d axonhub -c "ALTER USER axonhub WITH PASSWORD '${NEW_PASSWORD}';"

cp .env.example .env
sed -i "s/^DB_PASSWORD=.*/DB_PASSWORD=${NEW_PASSWORD}/" .env
docker compose up -d
```

The same applies to the commented-out MySQL services, whose previous defaults
were `axonhub_root_password` and `axonhub_password`.

Discarding the volume with `docker compose down -v` also works, but it deletes
every channel, key and request log, so keep that for throwaway deployments.

## Docker Compose Configuration

### Basic docker-compose.yml

```yaml
version: '3.8'

services:
  axonhub:
    image: looplj/axonhub:latest
    ports:
      - "8090:8090"
    volumes:
      - ./config.yml:/app/config.yml
      - axonhub_data:/app/data
    environment:
      - AXONHUB_SERVER_PORT=8090
      - AXONHUB_DB_DIALECT=sqlite3
      - AXONHUB_DB_DSN=file:axonhub.db?cache=shared&_fk=1
    restart: unless-stopped

volumes:
  axonhub_data:
```

### Production Configuration

```yaml
version: '3.8'

services:
  axonhub:
    image: looplj/axonhub:latest
    ports:
      - "8090:8090"
    volumes:
      - ./config.yml:/app/config.yml
      - axonhub_data:/app/data
      - ./logs:/app/logs
    environment:
      - AXONHUB_SERVER_PORT=8090
      - AXONHUB_DB_DIALECT=postgres
      - AXONHUB_DB_DSN=postgres://axonhub:password@postgres:5432/axonhub
      - AXONHUB_LOG_LEVEL=warn
      - AXONHUB_LOG_OUTPUT=file
      - AXONHUB_LOG_FILE_PATH=/app/logs/axonhub.log
    depends_on:
      - postgres
    restart: unless-stopped

  postgres:
    image: postgres:15
    environment:
      - POSTGRES_DB=axonhub
      - POSTGRES_USER=axonhub
      - POSTGRES_PASSWORD=password
    volumes:
      - postgres_data:/var/lib/postgresql/data
    restart: unless-stopped

volumes:
  axonhub_data:
  postgres_data:
```

## Database Options

### SQLite (Development)

```yaml
axonhub:
  environment:
    - AXONHUB_DB_DIALECT=sqlite3
    - AXONHUB_DB_DSN=file:axonhub.db?cache=shared&_fk=1&_pragma=journal_mode(WAL)
```

### PostgreSQL (Production)

```yaml
axonhub:
  environment:
    - AXONHUB_DB_DIALECT=postgres
    - AXONHUB_DB_DSN=postgres://user:pass@host:5432/axonhub
```

### MySQL (Production)

```yaml
axonhub:
  environment:
    - AXONHUB_DB_DIALECT=mysql
    - AXONHUB_DB_DSN=user:pass@tcp(host:3306)/axonhub?charset=utf8mb4&parseTime=True
```

## Environment Variables

### Server Configuration

```bash
AXONHUB_SERVER_PORT=8090
AXONHUB_SERVER_NAME="AxonHub"
AXONHUB_SERVER_DEBUG=false
AXONHUB_SERVER_REQUEST_TIMEOUT="30s"
AXONHUB_SERVER_LLM_REQUEST_TIMEOUT="600s"
```

### Database Configuration

```bash
AXONHUB_DB_DIALECT="postgres"
AXONHUB_DB_DSN="postgres://user:pass@host:5432/axonhub"
AXONHUB_DB_DEBUG=false
```

### Logging Configuration

```bash
AXONHUB_LOG_LEVEL="info"
AXONHUB_LOG_ENCODING="json"
AXONHUB_LOG_OUTPUT="stdio"
```

## Security Considerations

### Network Security

```yaml
axonhub:
  networks:
    - axonhub_network
  ports:
    - "127.0.0.1:8090:8090"  # Bind to localhost only

networks:
  axonhub_network:
    driver: bridge
```

### Secrets Management

Use Docker secrets or environment files:

```bash
# .env file
DB_PASSWORD=your-secure-password
API_KEY_SECRET=your-api-key-secret
```

```yaml
axonhub:
  env_file:
    - .env
```

## Monitoring and Logging

### Health Checks

```yaml
axonhub:
  healthcheck:
    test: ["CMD", "curl", "-f", "http://localhost:8090/health"]
    interval: 30s
    timeout: 10s
    retries: 3
    start_period: 40s
```

### Log Collection

```yaml
axonhub:
  logging:
    driver: "json-file"
    options:
      max-size: "10m"
      max-file: "3"
```

## Scaling

### Horizontal Scaling

```yaml
axonhub:
  deploy:
    replicas: 3
    resources:
      limits:
        memory: 1G
        cpus: '0.5'
      reservations:
        memory: 512M
        cpus: '0.25'
```

### Load Balancer Setup

```yaml
services:
  axonhub:
    image: looplj/axonhub:latest
    deploy:
      replicas: 3
    networks:
      - axonhub_network

  nginx:
    image: nginx:alpine
    ports:
      - "80:80"
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf
    depends_on:
      - axonhub
    networks:
      - axonhub_network
```

## Backup and Recovery

### Database Backup

```yaml
services:
  backup:
    image: postgres:15
    volumes:
      - ./backup:/backup
      - postgres_data:/var/lib/postgresql/data
    command: |
      bash -c '
        pg_dump -h postgres -U axonhub axonhub > /backup/axonhub-$(date +%Y%m%d).sql
      '
    depends_on:
      - postgres
    environment:
      - PGPASSWORD=password
```

### Volume Backup

```bash
# Backup data volume
docker run --rm -v axonhub_data:/source -v $(pwd)/backup:/backup alpine \
  tar czf /backup/axonhub-data-$(date +%Y%m%d).tar.gz -C /source .

# Restore data volume
docker run --rm -v axonhub_data:/target -v $(pwd)/backup:/backup alpine \
  tar xzf /backup/axonhub-data-20231110.tar.gz -C /target
```

## Troubleshooting

### Common Issues

**Container fails to start**
- Check Docker logs: `docker-compose logs axonhub`
- Verify configuration file permissions
- Ensure database connection is working

**Port conflicts**
- Change the exposed port in docker-compose.yml
- Check if another service is using port 8090

**Database connection issues**
- Verify database credentials
- Check network connectivity between containers
- Ensure database container is running

### Debug Mode

Enable debug logging for troubleshooting:

```yaml
axonhub:
  environment:
    - AXONHUB_SERVER_DEBUG=true
    - AXONHUB_LOG_LEVEL=debug
```

## Next Steps

- [Configuration Guide](configuration.md)
- [OpenAI API](../api-reference/openai-api.md)
- [Anthropic API](../api-reference/anthropic-api.md)
- [Gemini API](../api-reference/gemini-api.md)
- [Architecture Documentation](../development/erd.md)
