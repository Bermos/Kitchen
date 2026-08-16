# Changelog

## [0.3.0](https://github.com/Bermos/Kitchen/compare/v0.2.0...v0.3.0) (2026-08-16)


### Features

* **api:** answer what is actually running, not only what was asked for ([2eb3999](https://github.com/Bermos/Kitchen/commit/2eb39996f03a4271af3d419dd823f97ca191e7bf))
* **api:** ask the logs a question instead of writing SQL ([6e3bddc](https://github.com/Bermos/Kitchen/commit/6e3bddc8a494b589269df0f892374154027fef0c))
* **api:** close the write gaps for projects, connections, builds and previews ([641e5a7](https://github.com/Bermos/Kitchen/commit/641e5a729f46018cb60b8a9efab9b4fb6d2938d7))
* **api:** expose the platform's own upgrades ([d78bb79](https://github.com/Bermos/Kitchen/commit/d78bb79c139a19869af5e785aa02368134ccae90))
* **api:** refuse a Kitchen in acme mode that cannot get a certificate ([b4d6a08](https://github.com/Bermos/Kitchen/commit/b4d6a08c6994fe19a5f26396a02dea3b506e1821)), closes [#31](https://github.com/Bermos/Kitchen/issues/31)
* **chart:** keep a JSON log line's own fields ([2337894](https://github.com/Bermos/Kitchen/commit/233789467adda5da08a0e50bf30a38f926e3dedf))
* idle environments down to zero pods with KEDA ([#83](https://github.com/Bermos/Kitchen/issues/83)) ([c44218d](https://github.com/Bermos/Kitchen/commit/c44218deee5dc907ac3b4e0a61d908795cf03a90))
* **operator:** let the platform upgrade its own helm release ([fbcf440](https://github.com/Bermos/Kitchen/commit/fbcf440c447b4a0ec8bd8d02d101f8d3bf07feca))
* **operator:** tear down everything a deleted project owns ([12673e8](https://github.com/Bermos/Kitchen/commit/12673e883cae2056902b52b4ba68205172f80929))
* **ui:** drive every platform write from the dashboard ([6be1789](https://github.com/Bermos/Kitchen/commit/6be1789c30b643784ba417e111e62da78e6a55c5))
* **ui:** log analytics in the observability view ([3dca7c8](https://github.com/Bermos/Kitchen/commit/3dca7c856f47f577835de5952b2562a1fd9130f5))
* **ui:** offer the platform's own upgrade from the settings page ([5920f3d](https://github.com/Bermos/Kitchen/commit/5920f3dc717e68abd8751a686a6a5d667845fc98))
* **ui:** renew the dashboard session instead of bouncing through the login ([4dbaeba](https://github.com/Bermos/Kitchen/commit/4dbaeba4590f9ea6dc803908599e3ae46532661b)), closes [#25](https://github.com/Bermos/Kitchen/issues/25)
* **ui:** show the workload, the objects behind it and the platform's status ([9863bc2](https://github.com/Bermos/Kitchen/commit/9863bc276e5198b646b352b1246616066d69689b))


### Bug fixes

* **api:** stop the traffic query shadowing the column it filters on ([bafc5e3](https://github.com/Bermos/Kitchen/commit/bafc5e34c687d917f6335411022c76c3df466030)), closes [#57](https://github.com/Bermos/Kitchen/issues/57)
* **chart:** classify collected logs by namespace, not by missing labels ([a0e124e](https://github.com/Bermos/Kitchen/commit/a0e124eddf37e9848ca308d53d70347358dff34c))
* **operator:** ask the pods why a component is short, not only the workload ([fdc9e6c](https://github.com/Bermos/Kitchen/commit/fdc9e6cf5e9e8d92c5c826bd717e9278efbd407e)), closes [#55](https://github.com/Bermos/Kitchen/issues/55)


### Documentation

* point the happy path away from kubectl ([b47ffe3](https://github.com/Bermos/Kitchen/commit/b47ffe371b62b4b860b3b3a041aa66da7f795e76))

## [0.2.0](https://github.com/Bermos/Kitchen/compare/v0.1.4...v0.2.0) (2026-08-15)


### Features

* **ui:** the dashboard's telemetry: events, metrics, flows and live logs ([ae8ac29](https://github.com/Bermos/Kitchen/commit/ae8ac29501430c96d12e81dcf45d13f420f9710e))
* **ui:** show the release the operator was built from ([c7644aa](https://github.com/Bermos/Kitchen/commit/c7644aa816c8f536da24b468235135eb383c814d))
* record how a release stopped being current ([526976d](https://github.com/Bermos/Kitchen/commit/526976dc650300668c0bb0170e7f823495f951d8))


### Bug fixes

* **ci:** baseline release-please on v0.1.4, the newest tag that exists ([0d1a1a6](https://github.com/Bermos/Kitchen/commit/0d1a1a6977f5098f97afb5b88df67a711915942c))
* **ci:** name the updater for Chart.yaml so its comments survive a release ([067a40a](https://github.com/Bermos/Kitchen/commit/067a40abee17fe22b605b912932c6145a8adeb81))
* size the log collector for catching up, not for idling ([ea18664](https://github.com/Bermos/Kitchen/commit/ea1866451a7ffdafef030d4ca6e714d8a0f76953))


### Build and dependencies

* move to Go 1.26, the current release ([821bf5f](https://github.com/Bermos/Kitchen/commit/821bf5f4a7cb6ac24723fdd32c29c3d397d114b0))


### Documentation

* write down the commit conventions and the release path ([85813a3](https://github.com/Bermos/Kitchen/commit/85813a3a6d945b5f192d52d3a96af7efb588d0de))
