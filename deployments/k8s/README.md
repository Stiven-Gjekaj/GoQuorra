# Kubernetes manifests

These run GoQuorra on a cluster.
They are a starting point and not a production deployment.

The README of the version before the rebuild listed a `deployments/k8s`
directory that did not exist.
These files exist, and this page says plainly what they do not do.

## Apply them

    kubectl create namespace goquorra
    WORKER_KEY="$(openssl rand -hex 32)"
    kubectl -n goquorra create secret generic quorra \
      --from-literal=api-keys="ops:write:$(openssl rand -hex 32),fleet:worker:$WORKER_KEY" \
      --from-literal=worker-key="$WORKER_KEY" \
      --from-literal=database-url="postgres://user:password@host:5432/quorra?sslmode=require"
    kubectl -n goquorra apply -f .

Two keys and not one.
The worker protocol has a scope of its own, so a key an operator uses cannot
lease the queue empty and a worker cannot cancel anything.
`worker-key` is the secret half of the `fleet` entry in `api-keys`, which is
why it is written twice: the server needs the whole list and each worker needs
only its own secret.

The secret comes first.
The deployment does not start without it, because the server refuses to start
with no API key and the workers refuse to start with none.

## What these do not do

- **They do not run PostgreSQL.** Point `database-url` at a database you
  already operate. A queue keeps the only copy of work that has been accepted
  and not yet done, so it belongs on storage somebody backs up.
- **They do not apply the schema.** Apply every file in `migrations/`, in
  name order, before the first start. `make db-init` does that, and a Job that
  runs `psql -v ON_ERROR_STOP=1 -f` over each file in turn does the same thing
  in a cluster.

  Every file, and not the first one. This said `migrations/0001_init.sql`,
  which is the first of many. An operator who followed it got a `jobs` table with
  no cancel constraint, no idempotency key, no result column and no
  `acted_by`, and none of the four later tables. Measured: the first
  submission answered `500`, with `column "..." does not exist` in the log.

  Each file is written to be safe to apply twice, so a Job that runs again
  after a restart is not a problem.

  The server does not do this itself, and that is the decision: a migration
  that runs from inside the server means every replica races to apply it.
- **They set no resource limits that suit your load.** The numbers here are
  small enough to start on a laptop cluster and are not measurements of
  anything.
- **They do not scale the workers automatically.** Queue depth is published as
  `quorra_queue_length`, which is the number to scale on, and wiring that up
  needs an adapter this project does not ship.
