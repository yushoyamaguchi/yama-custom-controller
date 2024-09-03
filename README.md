# how to run
## custom resource
```
kubectl apply -f manifests/crd.yaml
kubectl apply -f manifests/deploy-myresource.yaml
```

## controller
```
go build -o simple-controller
./simple-controller 
```
