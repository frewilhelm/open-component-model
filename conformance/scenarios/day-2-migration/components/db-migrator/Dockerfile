FROM alpine:3.23

RUN apk add --no-cache postgresql16-client

ARG VERSION=1.0.0
ARG MIGRATION_DIR=v1

COPY scripts/migrate.sh /scripts/migrate.sh
RUN chmod +x /scripts/migrate.sh

RUN mkdir -p /migrations
COPY migrations/${MIGRATION_DIR}/ /migrations/

USER nobody

ENTRYPOINT ["/bin/sh", "/scripts/migrate.sh"]
