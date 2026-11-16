# HTTP 协议

所有业务路径以 `/v1` 开头。请求体使用 JSON，素材上传直接使用 PNG 字节。

| 方法和路径 | 请求 | 结果 |
|---|---|---|
| PUT /scenes/{id} | name, expectedRevision, document | 完整场景，200 |
| GET /scenes/{id} | 无 | 完整场景，200 |
| POST /scenes/{id}/state | expectedRevision, state | 完整场景，200 |
| POST /scenes/{id}/snapshots | expectedRevision | 快照和素材闭包，201 |
| PUT /assets/{id} | PNG字节 | 素材及base64字节，201 |
| POST /assets/{id}/retire | 无 | state，200 |
| GET /snapshots/{id} | 无 | 快照，200 |
| POST /snapshots/{id}/revoke | 无 | state，200 |
| GET /snapshots/{id}/package | 无 | atlas-scene交换包，200 |
| POST /imports/{id} | name, package | 新场景，201 |
| POST /jobs | snapshotId, scale；请求头Idempotency-Key | 任务，202 |
| GET /jobs/{id} | 无 | 任务，200 |
| POST /jobs/{id}/cancel | 无 | 任务，200 |
| POST /jobs/{id}/retry | 无 | 任务，202 |
| GET /artifacts/{id} | 可选If-None-Match | PNG，200或304 |
| DELETE /artifacts/{id} | 无 | state，200 |
| POST /compare | left, right（结果ID） | 像素比较，200 |
| GET /status | 无 | 场景/素材/快照/任务计数、结果字节数和 `disk` 卷状态 |
| GET /events | 无 | 最多500个有序领域事件 |

所有需要持久提交的入口在状态盘空间不足时返回 **503**，JSON 形如 `{"code":"disk_full","error":"..."}` 并带 `Retry-After: 30` 响应头。503 表示事务在任何业务变更之前被拒绝，没有任何承诺落盘；客户端应保持原幂等键稍后重试。只读入口（GET、结果下载、`/v1/compare`）不受影响。`DELETE /artifacts/{id}` 是例外：它只缩小状态，满盘时仍可提交，用于释放空间。`disk` 块字段为 `writable`、`reason`、`faultInjected`、`path`、`freeBytes`、`totalBytes`、`freeInodes`、`marginBytes`、`marginInodes`、`checkedAt`。

`GET /healthz` 不带 `/v1`，可写时返回200并附 `disk` 状态；状态盘低于预留水位时返回503，响应体 `status` 为 `degraded` 且包含同样的 `disk` 详情。它表明进程已打开并恢复状态，磁盘探测直接 `statfs` 状态目录，不做试探性写入。任务响应省略固定输入的有效载荷，输入细节通过快照接口读取。

文档字段：width、height、background、layers。每个图层有id、kind、width、height、transform、opacity、fill；transform包括x、y、scaleX、scaleY、degrees。图层可以有clip（x/y/width/height），group使用children，polygon使用points，image使用assetId。solid颜料有color；linear还包括endColor、from、to。示例是可直接提交的完整请求。

幂等键长度1–80，使用英文字母、数字、下划线和连字符，首位不能是后两者。它在服务实例持久状态中全局生效，不随任务完成、取消、结果清理或进程重启失效。改换请求语义需使用新键。

服务接受相同内容的素材重复上传，退役素材不接受重新上传。交换包导出保留快照状态；导入会创建全新的active场景，原快照不作为目标服务中的快照记录落地。导入目标ID必须未使用；共享素材必须内容相同且仍可用。
