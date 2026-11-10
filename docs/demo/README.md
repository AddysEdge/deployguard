# Recorded published-binary demo

This snapshot came from [workflow run 42758992185](https://github.com/AddysEdge/deployguard/actions/runs/42758992185).
Its Linux, Windows and macOS jobs all passed. The committed report files below are
the Linux job's output, downloaded without changing their contents.

- [evidence.json](evidence.json): actual binary hash, version, platform, workflow,
  wrapper commit and observed exit codes.
- [workload.yaml](workload.yaml): the single declared request.
- [clean.json](clean.json): a harmless JSON key reorder, observed PASS / exit 0.
- [regressed.json](regressed.json): removal of the email field, observed FAIL / exit 1.
- [transcript.txt](transcript.txt): complete recorded command labels, stdout and stderr.
- [terminal.svg](terminal.svg): a readable rendering with INFO logs omitted and
  long lines wrapped. Command labels are abbreviated; the actual argument list is
  assembled in the [demo script](../../scripts/demo-release.mjs).

These are synthetic loopback fixtures and two functional-contract checks. They do
not establish general detection accuracy, production latency or real-user adoption.
The separate seeded evaluation covers additional cases.

To produce another snapshot, use the [download/demo guide](../downloads.md), then
run `node scripts/render-demo.mjs <output directory>`. Preserve the original
reports and metadata when publishing a new measurement.
