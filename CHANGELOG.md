# Changelog

## [0.4.0](https://github.com/Bermos/Kitchen/compare/v0.3.0...v0.4.0) (2026-08-16)


### Features

* **api:** attach and detach custom domains ([ab00c6e](https://github.com/Bermos/Kitchen/commit/ab00c6ea29912958eb5b38941e53bf9436ef4c0c))
* **api:** create and delete resource claims ([98b1791](https://github.com/Bermos/Kitchen/commit/98b17914edd9727187b61743ca2fd72116525d94))
* **api:** name a query worth keeping ([734f3a8](https://github.com/Bermos/Kitchen/commit/734f3a828151c4ec1d48c2633b4322dc3609a1c2))
* **api:** serve resource history and traces ([7036059](https://github.com/Bermos/Kitchen/commit/70360598763030a8a6b6ae2651cf66a329f5b0b2))
* **api:** test a connection's credential without storing it ([e54b983](https://github.com/Bermos/Kitchen/commit/e54b983147d19deae9607a2e9988f9100c538491))
* **api:** warn when a connection's token cannot report deploys ([18afc14](https://github.com/Bermos/Kitchen/commit/18afc1491da2b6207352b1c674cdfe5cc78951bb))
* **chart:** run the trace receiver and lift trace ids out of log lines ([2fd9408](https://github.com/Bermos/Kitchen/commit/2fd9408397f3c7ed510d389c29ef065cde2f2b1e))
* **chart:** switch cert-manager's Gateway API support on ([8f5d5d6](https://github.com/Bermos/Kitchen/commit/8f5d5d6d9b7ef6f9a3e94298225e528324d8b54f))
* **operator:** add a generic database provisioner with a neon implementation ([c53ec36](https://github.com/Bermos/Kitchen/commit/c53ec36a5f86e28ceeb7d1ebde117fc769b7d990))
* **operator:** bind resource claims and branch databases per preview ([bb195e0](https://github.com/Bermos/Kitchen/commit/bb195e0f14e8c07a05a63925aec4b9d892bd6b01))
* **operator:** collect resource usage and traces into the store ([e13251e](https://github.com/Bermos/Kitchen/commit/e13251e018a9cc66bc45c106006916bf5560cf10))
* **operator:** validate connection credentials against their providers ([d1abb82](https://github.com/Bermos/Kitchen/commit/d1abb82afc6f0cdb2ef37d19e2a577b958f83628))
* **operator:** verify, certify and route custom domains ([c816be5](https://github.com/Bermos/Kitchen/commit/c816be5053a0c4ed2de5e7983df6d31cfbf2e473))
* **ui:** attach custom domains from the environment screen ([f9517dc](https://github.com/Bermos/Kitchen/commit/f9517dc29f0277ec17773cf65f6ad0e2a5916fdd))
* **ui:** keep a question, and find it again ([0f74d5a](https://github.com/Bermos/Kitchen/commit/0f74d5a9bc3be334044ada4b5c0e632a19df9c00))
* **ui:** make a broken connection legible on the connections page ([13dff26](https://github.com/Bermos/Kitchen/commit/13dff26526d1494822b0df048a64446a59d6a3ed))
* **ui:** manage resource claims from the project screen ([4af3527](https://github.com/Bermos/Kitchen/commit/4af3527ec4bcf61c4e23f15d24d4a83a0b0f81fe))
* **ui:** say what a connection's token needs, and test it ([610abba](https://github.com/Bermos/Kitchen/commit/610abba25875d8603d8703e5d057c7aca020d13d))
* **ui:** show an environment's history and the requests behind it ([e1ef0af](https://github.com/Bermos/Kitchen/commit/e1ef0af57a42413fa1d8e35044755365987ed523))


### Bug fixes

* **api:** serialise an empty log facet as a list, not null ([d6f65c5](https://github.com/Bermos/Kitchen/commit/d6f65c59d92bd873add255a3907d2299baf4f444))
* **operator:** keep sampling and traces on through an upgrade ([b8f7ff2](https://github.com/Bermos/Kitchen/commit/b8f7ff288b4ff9127179217810033b797b1cece8))
* **operator:** let the operator create the database it was pointed at ([9dc4981](https://github.com/Bermos/Kitchen/commit/9dc49817a60acdae56f86ea347f0eaa05c279847))


### Documentation

* count the connection reconciler among the ones that exist ([99c6e93](https://github.com/Bermos/Kitchen/commit/99c6e935b6b3192631ba6a217325861b4eb2d702))
* stop calling the reconcilers stubs ([53dda81](https://github.com/Bermos/Kitchen/commit/53dda8102a94b4c75dbac5725d7eb3f48100c5cc))
* write down the telemetry the store actually holds now ([1277e63](https://github.com/Bermos/Kitchen/commit/1277e6317a432d17d84ec7f31a5192deccdc588d))

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
