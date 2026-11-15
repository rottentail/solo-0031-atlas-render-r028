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
| POST /jobs/{id}/reruns | mode、current模式必填expectedRevision；请求头Idempotency-Key | 新任务，202 |
| GET /artifacts/{id} | 可选If-None-Match | PNG，200或304 |
| DELETE /artifacts/{id} | 无 | state，200 |
| POST /compare | left, right（结果ID） | 像素比较，200 |
| GET /status | 无 | 场景/素材/快照/任务计数和结果字节数 |
| GET /events | 无 | 最多500个有序领域事件 |

`GET /healthz` 不带 `/v1`，返回ready。它表明进程已打开并恢复状态，不包含单独的磁盘可写探测。任务响应省略固定输入的有效载荷，输入细节通过快照接口读取。

文档字段：width、height、background、layers。每个图层有id、kind、width、height、transform、opacity、fill；transform包括x、y、scaleX、scaleY、degrees。图层可以有clip（x/y/width/height），group使用children，polygon使用points，image使用assetId。solid颜料有color；linear还包括endColor、from、to。示例是可直接提交的完整请求。

幂等键长度1–80，使用英文字母、数字、下划线和连字符，首位不能是后两者。它在服务实例持久状态中全局生效，不随任务完成、取消、结果清理或进程重启失效。改换请求语义需使用新键。

取消或失败后的再次执行提供两条语义明确的路径，调用方必须显式选择，服务端不会替调用方猜测该用哪个版本：

- `POST /jobs/{id}/retry` 保持原意：在原任务上递增 `attempt`，沿用提交时冻结的同一份快照输入，不读取场景的后续编辑。重复调用只在同一任务上继续推进，不产生新任务。
- `POST /jobs/{id}/reruns` 创建一个新的执行任务，请求体 `mode` 取 `frozen` 或 `current`：
  - `frozen`：新任务沿用源任务被取消（或失败）时冻结的同一份快照，源快照若已撤回则返回409；
  - `current`：必须显式给出 `expectedRevision`，服务端在同一事务内冻结该场景当前修订并渲染新版本，修订不匹配返回409，因此不会出现“以为用了最新编辑、实际用了旧内容”的歧义。

reruns 与提交任务一样强制携带 `Idempotency-Key`：同键同语义重放同一新任务（重复点击、连接重试、进程重启都不多出执行承诺），同键异语义返回409。源任务保持 `cancelled`/`failed` 终态且不被修改，新任务独立进入队列，取消与完成不可能各写一份结果。

每个任务携带 `lineage` 血缘对象：`rootId` 是尝试链根任务，`parentId` 是直接来源任务，`depth` 从0递增，`source` 为 `submit`、`retry` 或 `rerun`，`mode` 记录 rerun 的 `frozen`/`current` 选择。另有 `job.rerun` 事件记录新任务的产生，配合事件接口可还原完整尝试关系。持久化状态中早于血缘字段的旧任务会在启动时回填为自身根。

服务接受相同内容的素材重复上传，退役素材不接受重新上传。交换包导出保留快照状态；导入会创建全新的active场景，原快照不作为目标服务中的快照记录落地。导入目标ID必须未使用；共享素材必须内容相同且仍可用。
