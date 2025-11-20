#!/bin/bash

NAMESPACE="nexslice"
UE_POD=$(kubectl get pods -n $NAMESPACE --no-headers | grep ueransim-gnb-ues | awk '{print $1}')

for i in $(seq 0 99); do
  echo "Pinging from uesimtun$i ..."
  kubectl exec -n $NAMESPACE $UE_POD -- ping -I uesimtun$i -c 3 google.com &
  sleep 1
done

wait
echo "Done."