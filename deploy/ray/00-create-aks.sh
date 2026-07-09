#!/usr/bin/env bash
# Create an AKS cluster with a 2-node H100 GPU pool for Ray Serve LLM.
# Run manually; edit the vars first. Requires: az CLI, kubectl.
set -euo pipefail

RG="${RG:-h100-ray-rg}"
LOCATION="${LOCATION:-eastus2}"
CLUSTER="${CLUSTER:-kimi-ray}"
GPU_SKU="${GPU_SKU:-Standard_ND96isr_H100_v5}"   # 8x H100 80GB SXM + InfiniBand
GPU_COUNT="${GPU_COUNT:-2}"                        # 2 nodes -> 16 GPUs total

# 1. Resource group.
az group create -n "$RG" -l "$LOCATION"

# 2. Cluster with a small CPU system pool (hosts the Ray head).
az aks create -g "$RG" -n "$CLUSTER" \
  --node-count 2 \
  --node-vm-size Standard_D8s_v5 \
  --nodepool-name system \
  --generate-ssh-keys

# 3. H100 GPU pool. --gpu-driver None: driver comes from the GPU Operator.
az aks nodepool add -g "$RG" --cluster-name "$CLUSTER" \
  --name h100 \
  --node-vm-size "$GPU_SKU" \
  --node-count "$GPU_COUNT" \
  --gpu-driver None \
  --node-taints sku=gpu:NoSchedule \
  --labels agentpool=h100

# 4. Credentials.
az aks get-credentials -g "$RG" -n "$CLUSTER" --overwrite-existing
kubectl get nodes -o wide
