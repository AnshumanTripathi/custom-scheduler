#!/bin/bash

echo "Creating KinD cluster"
kind create cluster --config KinD.yaml

echo "Building scheduler..."
cd plugins/ha-scheduler
docker build -t ha-scheduler:latest .

echo "Loading image in the kind cluster..."
kind load docker-image ha-scheduler:latest --name scheduler-cluster

cd ../../
echo "Deploying scheduler to the cluster..."
kubectl apply -f sa.yaml
kubectl apply -f rbac.yaml
kubectl apply -f cm.yaml
kubectl apply -f deployment.yaml

echo "Creating test pods..."
kubectl apply -f pod1.yaml
kubectl apply -f pod2.yaml
