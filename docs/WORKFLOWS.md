# Bounded expert workflows

A workflow is an explicit directed acyclic graph of independently configured model-agent tasks. Each step names an available model profile and may reference earlier step outputs. Cycles, unknown models, missing dependencies, oversized prompts and too many steps are rejected **before execution**.

Example request (`WW_TOKEN` is a user-supplied local secret):

```bash
curl -N -X POST http://127.0.0.1:8787/api/workflows/execute \
  -H "Authorization: Bearer $WW_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"Engineering review","steps":[
     {"id":"architecture","model":"demo","task":"Propose a small, testable architecture."},
     {"id":"verification","model":"demo","task":"Design unit and integration tests for this architecture.","depends_on":["architecture"]},
     {"id":"risk","model":"demo","task":"Identify the top risks.","depends_on":["architecture","verification"]}
  ]}'
```

The response is a typed SSE feed with `workflow.started`, `workflow.step.started`, `workflow.step.delta`, `workflow.step.completed` and `workflow.completed` events. The run is saved in `.ww-data/workflows/{id}.json` after each stage and can be inspected using `GET /api/workflows` and `GET /api/workflows/{id}`.

**Current limitations:** execution is dependency ordered, not parallel; workflow steps only call models, not tools. Interrupted jobs are retained as interrupted with completed outputs rather than automatically resumed or reissued. Durable queues, parallel DAG scheduling, checkpoints and scoped tool use are future work. The managed chat loop separately supports tool calls, approvals, delegation and cancellation.
