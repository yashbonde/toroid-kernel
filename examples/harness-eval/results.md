# Harness eval: toroid vs claude vs pi

Same task, same model (`llmgateway/glm-5p2` via Razorpay gateway), isolated clones.
Total cost uses one shared rate (in $0.60 / out $2.20 / cacheRead $0.11 per 1M tok).

| metric | toroid | claude | pi |
|---|---|---|---|
| success /5 | 5 | 4 | 0 |
| wall time (s) | 288.3 | 256.2 | 141.9 |
| CPU time (s) | 18.3 | 37.3 | 2.5 |
| peak RAM (MB) | 2546 | 557 | 246 |
| mean RAM (MB) | 34 | 436 | 177 |
| peak procs | 14 | 6 | 3 |
| LLM turns | 45 | 52 | 10 |
| tool calls | 45 | 51 | 9 |
| input tok | 46158 | 34400 | 38302 |
| output tok | 13644 | 11280 | 8857 |
| cache read tok | 1146375 | 273702 | 79638 |
| total tok | 1206177 | 319382 | 126797 |
| out tok/s | 47.3 | 44.0 | 62.4 |
| total cost ($) | 0.1838 | 0.0756 | 0.0512 |
| self-reported cost ($) | 0.4258 | 0.5909 | 0.0512 |
| ✓ branch_pushed | yes | yes | no |
| ✓ committed | yes | yes | no |
| ✓ kernel_changed | yes | yes | no |
| ✓ builds | yes | yes | no |
| ✓ bench_log | yes | no | no |

