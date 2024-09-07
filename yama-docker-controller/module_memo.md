# なぜk8s/sample-controllerをimportすると、pkg以下のソースコードから生成されたものをライブラリとして利用できるのか？
```
module k8s.io/sample-controller
```
go.mod内のこの記述により、このプロジェクト全体がk8s.io/sample-controllerというモジュールとして扱われるようになる。