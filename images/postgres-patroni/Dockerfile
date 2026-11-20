# PostgreSQL 16 with Patroni, prepared for the Kubernetes DCS.
FROM postgres:16-bookworm

USER root

RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip python3-psycopg2 \
    && pip3 install --no-cache-dir --break-system-packages "patroni==4.0.4" \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /home/postgres/pgdata \
    && chown -R postgres:postgres /home/postgres

USER postgres

ENTRYPOINT ["patroni"]
CMD ["/config/patroni.yml"]
