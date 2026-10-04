#!/bin/sh

# --development makes the operator use --kubeconfig, not the in-cluster configuration.
./scripts/build.sh && exec ./bin/redis-operator --development --kubeconfig=/.kube/config
