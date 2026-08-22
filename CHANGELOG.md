# Changelog

## [0.12.0](https://github.com/Bermos/Kitchen/compare/v0.11.0...v0.12.0) (2026-08-22)


### Features

* **operator:** data classification and residency as first-class fields ([#137](https://github.com/Bermos/Kitchen/issues/137)) ([8d9b9f3](https://github.com/Bermos/Kitchen/commit/8d9b9f3f909c99fdaef4f759350455c2cfcab9a5))
* **operator:** embedded OPA policy engine with reproducible stored decisions ([#132](https://github.com/Bermos/Kitchen/issues/132)) ([69363df](https://github.com/Bermos/Kitchen/commit/69363df3e11d98a22cf0f104d8c3d772d40e9dab))
* **operator:** environment ownership and eligibility requirements ([#131](https://github.com/Bermos/Kitchen/issues/131)) ([644c02c](https://github.com/Bermos/Kitchen/commit/644c02c98441699e476185d08595a15936c7e366))
* **operator:** exception and break-glass object with expiry and escalation ([#136](https://github.com/Bermos/Kitchen/issues/136)) ([d8d21f2](https://github.com/Bermos/Kitchen/commit/d8d21f29d0ae813d17020c115dbb5accda136bae))
* **operator:** resource claim provider contract with data-class attestation ([#138](https://github.com/Bermos/Kitchen/issues/138)) ([4db593d](https://github.com/Bermos/Kitchen/commit/4db593d566aec8cd713c635152e1baef3b5b395b))
* **operator:** staged promotion pipeline with policy-gated stages ([#133](https://github.com/Bermos/Kitchen/issues/133)) ([2a7babb](https://github.com/Bermos/Kitchen/commit/2a7babb5812194e6eba58f362067b548ba50f7d3))


### Bug fixes

* **operator:** clone private repositories with the source connection's token ([8a64253](https://github.com/Bermos/Kitchen/commit/8a64253afd8d6fcb148acdf2963a2f89b1831bb2))
* **operator:** close the seams between the compliance issues ([#144](https://github.com/Bermos/Kitchen/issues/144)) ([543c6bc](https://github.com/Bermos/Kitchen/commit/543c6bc879b470137ef6ed741e62ba211c444d1c))


### Documentation

* walk the diff once before a pull request opens ([5cacded](https://github.com/Bermos/Kitchen/commit/5cacdedb876c9506a85940d370a92407c07d3c69))

## [0.11.0](https://github.com/Bermos/Kitchen/compare/v0.10.0...v0.11.0) (2026-08-21)


### Features

* **api:** detect a repository's layout before the project exists ([878bf47](https://github.com/Bermos/Kitchen/commit/878bf4732d6d8f6565e89498e9f4dd6ceca94ca5))
* **cli:** create a project from the command line ([d255777](https://github.com/Bermos/Kitchen/commit/d255777d9554c36167b85f5fbbe6c6ccd69df627))
* **operator:** build the production branch when a project is created ([c1e811a](https://github.com/Bermos/Kitchen/commit/c1e811a2158935eab766a32138bdcf164acdc8e3))
* **ui:** show the detected layout while creating a project ([ca9c37f](https://github.com/Bermos/Kitchen/commit/ca9c37fee87a717d22b0fbef196813bbc3ff4b96))


### Documentation

* **cli:** document creating a project from the command line ([8b0c71b](https://github.com/Bermos/Kitchen/commit/8b0c71ba709760bb5bb185f76d63389af24a4663))

## [0.10.0](https://github.com/Bermos/Kitchen/compare/v0.9.0...v0.10.0) (2026-08-21)


### Features

* **api:** list a git connection's repositories for the project form ([#164](https://github.com/Bermos/Kitchen/issues/164)) ([c1371fe](https://github.com/Bermos/Kitchen/commit/c1371fe9bed458677fbd3375e63dd6bdb476b5dc))

## [0.9.0](https://github.com/Bermos/Kitchen/compare/v0.8.0...v0.9.0) (2026-08-20)


### ⚠ BREAKING CHANGES

* **api:** the REST API now enforces the developer/operator split from docs/AUTH.md. A token that could call every route can now call what its account's roles allow. Installations upgrading into this have their operator list seeded from the accounts that exist, so nothing locks itself out — that list should be reviewed and narrowed.

### Features

* **api:** enforce the role model, and tell the dashboard what it may do ([8241e11](https://github.com/Bermos/Kitchen/commit/8241e112c2fe321c0a6799effc8fbd3a79d30f62))
* **api:** make a CI key a member of one project and nothing else ([4c5b3fa](https://github.com/Bermos/Kitchen/commit/4c5b3fa46c7ff3d6261a839ae4571f00a1be7857)), closes [#111](https://github.com/Bermos/Kitchen/issues/111)
* **api:** make projects self-service, and let their admins manage membership ([d0d62a6](https://github.com/Bermos/Kitchen/commit/d0d62a629dfbeac83c6998299a9ce74ef1e8a8de)), closes [#106](https://github.com/Bermos/Kitchen/issues/106)
* **backup:** back the platform up, and restore it ([669a07f](https://github.com/Bermos/Kitchen/commit/669a07f201800e30db3c81be256ef84c127fa9a8)), closes [#74](https://github.com/Bermos/Kitchen/issues/74)
* **cli:** kitchen backup ([629b819](https://github.com/Bermos/Kitchen/commit/629b819441ea90f7de7ad6e943dc3c6a16193416))
* **cli:** kitchen link, deploy, logs, env and rollback, built to be driven ([8b51813](https://github.com/Bermos/Kitchen/commit/8b518134b2cbf5fc1c839b5423e9b6145ba44794)), closes [#76](https://github.com/Bermos/Kitchen/issues/76)
* **gate:** admit a protected preview by project membership, not by being signed in ([5f71367](https://github.com/Bermos/Kitchen/commit/5f71367cfb4963e15eb1073e65ac263f59123dd6)), closes [#110](https://github.com/Bermos/Kitchen/issues/110)
* implement the developer/operator split ([c90d526](https://github.com/Bermos/Kitchen/commit/c90d526a1e1ef1dfc6d28bc18f9277f1cbb32e23))
* **operator:** app sign-on with a ResourceClaim of type oidcClient ([#161](https://github.com/Bermos/Kitchen/issues/161)) ([facf601](https://github.com/Bermos/Kitchen/commit/facf6013fa0d9c73d826588e17234b2e1dfc3468))
* **operator:** ask the builder for provenance and a bill of materials ([acd15d3](https://github.com/Bermos/Kitchen/commit/acd15d39d3e528257f706f6efca21c5b8178f89a)), closes [#128](https://github.com/Bermos/Kitchen/issues/128)
* **operator:** establish how a commit was reviewed, and refuse one that was not ([a320cdb](https://github.com/Bermos/Kitchen/commit/a320cdbc81d4b1bb47eeacedb6999008630f2cde)), closes [#129](https://github.com/Bermos/Kitchen/issues/129)
* **operator:** install KEDA and its HTTP add-on for the platform ([29d5b48](https://github.com/Bermos/Kitchen/commit/29d5b48aa73dc098d5ec09d4a5d7164753392b73))
* **operator:** name the first operator, and grandfather the installs that upgrade into enforcement ([ea841c5](https://github.com/Bermos/Kitchen/commit/ea841c51dc2013f57873c254255d7667895485f3)), closes [#104](https://github.com/Bermos/Kitchen/issues/104)
* **operator:** reuse build layers through a cache in the connected registry ([5cf01f6](https://github.com/Bermos/Kitchen/commit/5cf01f60e9ac45a42ae8f39a0a7f436dfd139b6e)), closes [#70](https://github.com/Bermos/Kitchen/issues/70)
* **operator:** run quality gates over every artifact and sign what they find ([f7f3906](https://github.com/Bermos/Kitchen/commit/f7f390647e8dff15605b2cb35177cd04502ec094)), closes [#130](https://github.com/Bermos/Kitchen/issues/130)
* **ui:** a project's people and keys, its variables, and the platform's operators ([258e734](https://github.com/Bermos/Kitchen/commit/258e734be59a21000c466a7d9e0d174dfd765f5b))
* **ui:** make the dashboard follow the role, and default the mode from it ([0e33de1](https://github.com/Bermos/Kitchen/commit/0e33de10405afb62eb78ad9cb24c5d628bbea6cf))


### Bug fixes

* **api:** move three surfaces to the role the model gives them ([0fff41b](https://github.com/Bermos/Kitchen/commit/0fff41b499e92a3e8379df13610841bf5a527ed9))
* **api:** stop the enforcement pass leaking across projects, and read a claim the way the gate does ([f171657](https://github.com/Bermos/Kitchen/commit/f17165710ac4db6ef07fff3bd41947d47af97272))
* **chart:** hash the registry password the Secret publishes ([166a580](https://github.com/Bermos/Kitchen/commit/166a580da4f7cf0696e4c94173af8b56ca9a5153))
* **chart:** keep a restore's archive off the image's own binaries ([edbeac7](https://github.com/Bermos/Kitchen/commit/edbeac7f324ccd08a3148dbec17da757d8c05f9a))
* **chart:** let an installation name its operators, and stop an upgrade re-granting the platform ([392a719](https://github.com/Bermos/Kitchen/commit/392a719b1a56c75e4060097ba2bbd95cbaef2f43))
* **ci:** publish the GitHub release only once the artifacts exist ([d8749da](https://github.com/Bermos/Kitchen/commit/d8749da61956048ee486ff925bb7d9f15bdae5c8))
* **ui:** let a viewer read a project's variables, and regenerate the CRDs ([0dbf692](https://github.com/Bermos/Kitchen/commit/0dbf69214c3cc6811ddd1b30776f16878ff1d8d8))


### Documentation

* **auth:** bring the surface table in line with what shipped ([ceab0db](https://github.com/Bermos/Kitchen/commit/ceab0db71ddb125b7fdadee3501d412c8deec294))
* correct three things a review found saying the wrong thing ([950e26d](https://github.com/Bermos/Kitchen/commit/950e26d5f7b9e5b7e93a824330dab695b9581e9e))
* say that the role model is enforced, and correct what it says about keys ([c4499d8](https://github.com/Bermos/Kitchen/commit/c4499d8cd78cfa377aa290317df9cdd55dcb3374))


### Build and dependencies

* **ui:** generate the dashboard's copy of the role table from the API's ([c3d328f](https://github.com/Bermos/Kitchen/commit/c3d328face16db054646a1b1f17e3d06c47157d4)), closes [#108](https://github.com/Bermos/Kitchen/issues/108)

## [0.8.0](https://github.com/Bermos/Kitchen/compare/v0.7.0...v0.8.0) (2026-08-19)


### Features

* **api:** re-check the published versions on demand ([b3e810f](https://github.com/Bermos/Kitchen/commit/b3e810fc25b9956998b00757e8a837991d27916c))
* **ui:** check for platform updates without waiting out the cache ([af03337](https://github.com/Bermos/Kitchen/commit/af03337962db8205d818485fcdc872736d40fffc))

## [0.7.0](https://github.com/Bermos/Kitchen/compare/v0.6.0...v0.7.0) (2026-08-19)


### Features

* **operator:** record what the platform did, and sign what it built ([d8f874e](https://github.com/Bermos/Kitchen/commit/d8f874eebbfa424f2f2a7289ca07a3abab0ff561))
* **ui:** make the dashboard usable on small screens ([4758135](https://github.com/Bermos/Kitchen/commit/47581351273cbf6054f97f57052dff04529e38c1))


### Build and dependencies

* **auth:** cross-build the arm64 image instead of emulating it ([b81c17a](https://github.com/Bermos/Kitchen/commit/b81c17a0c74fd345091bd371de516e0fa535eb69))

## [0.6.0](https://github.com/Bermos/Kitchen/compare/v0.5.1...v0.6.0) (2026-08-19)


### Features

* **api:** configure the registry the platform runs for itself ([732cee1](https://github.com/Bermos/Kitchen/commit/732cee10304ff77444512393b7d9d9bf80e1e1b6)), closes [#122](https://github.com/Bermos/Kitchen/issues/122)
* **chart:** run zot as the platform's image registry ([69e1f6f](https://github.com/Bermos/Kitchen/commit/69e1f6f37003dc013519dfb42a6771753f887044)), closes [#122](https://github.com/Bermos/Kitchen/issues/122)
* **operator:** answer where a release is live, and bound how many a project keeps ([#125](https://github.com/Bermos/Kitchen/issues/125)) ([507d38a](https://github.com/Bermos/Kitchen/commit/507d38a34adb6bf0b0bd2ef8ad8de6e7635ce2f0)), closes [#68](https://github.com/Bermos/Kitchen/issues/68)
* **operator:** build with Cloud Native Buildpacks ([462b02c](https://github.com/Bermos/Kitchen/commit/462b02c91f8d10fe780740627e9883406d2a56d0)), closes [#69](https://github.com/Bermos/Kitchen/issues/69)
* **operator:** detect the framework a repository is built with ([c6092ba](https://github.com/Bermos/Kitchen/commit/c6092ba57fddec926f7c8ee2afdab8015e9183f5)), closes [#69](https://github.com/Bermos/Kitchen/issues/69)
* **operator:** publish the bundled registry and seed its connection ([525abe9](https://github.com/Bermos/Kitchen/commit/525abe97557566f33eef2905bc4c87923255ae33)), closes [#122](https://github.com/Bermos/Kitchen/issues/122)
* **ui:** preselect a connection when there is only one to pick ([adab9fa](https://github.com/Bermos/Kitchen/commit/adab9faf1fd72c58fcd646229424e6232a1dcf82)), closes [#122](https://github.com/Bermos/Kitchen/issues/122)


### Bug fixes

* **chart:** give zot a tag pattern it will actually start on ([e6961ea](https://github.com/Bermos/Kitchen/commit/e6961eacc798ca39b1358ed453104a992e5d3ffc)), closes [#122](https://github.com/Bermos/Kitchen/issues/122)
* **operator:** pull application images with the registry credential ([#117](https://github.com/Bermos/Kitchen/issues/117)) ([c90a863](https://github.com/Bermos/Kitchen/commit/c90a863a0172211a9f019d5dd293e622fba84f4f))
* **operator:** stop reporting the platform's own URL as unrouted traffic ([1a5d269](https://github.com/Bermos/Kitchen/commit/1a5d269234058bfe3b88fb9075a29f5e457550cf))


### Documentation

* write down how a build's framework is detected ([47894f8](https://github.com/Bermos/Kitchen/commit/47894f8200a5de9232aae5d750b599d09d795199))
* write down the bundled registry and why it faces outward ([b783ad8](https://github.com/Bermos/Kitchen/commit/b783ad8d26cd9e744510aa26b3d2caab1894d934)), closes [#122](https://github.com/Bermos/Kitchen/issues/122)

## [0.5.1](https://github.com/Bermos/Kitchen/compare/v0.5.0...v0.5.1) (2026-08-18)


### Bug fixes

* **chart:** label the collector's pods as part of kitchen ([fe1311f](https://github.com/Bermos/Kitchen/commit/fe1311f71bd435ed4834c6be05c20f6d01961dea))
* **operator:** give the gate's probes a moment to lose the start-up race ([02035a5](https://github.com/Bermos/Kitchen/commit/02035a5d9d1aa2afa2d7e93641a7b36e313563ee))


### Documentation

* write down the developer/operator split, and give the personas a home ([#112](https://github.com/Bermos/Kitchen/issues/112)) ([65aa6af](https://github.com/Bermos/Kitchen/commit/65aa6af116af2b37a1f2cb57fad8447633d40967))

## [0.5.0](https://github.com/Bermos/Kitchen/compare/v0.4.0...v0.5.0) (2026-08-17)


### ⚠ BREAKING CHANGES

* the collector's ClusterRole now needs nodes/proxy and nodes/pods. Naming the claim behind a filling volume requires the kubelet's /pods endpoint, and a 403 there fails the whole kubelet scrape — CPU and memory collection stop along with the volume metrics. Installs that set collector.rbac.create=false and supply the role themselves must add both grants before upgrading.
* **chart:** the `logs.*` values are replaced by `collector.*`. The chart README carries the old-to-new mapping. Installs that never overrode them need no change.

### Features

* **chart:** run one OpenTelemetry collector instead of Vector ([9bfe228](https://github.com/Bermos/Kitchen/commit/9bfe22865a71579756aa1bc0aa8438bf858aa8ec))
* observe every request the edge serves, and tell the operator what is wrong ([#98](https://github.com/Bermos/Kitchen/issues/98)) ([bc2eb73](https://github.com/Bermos/Kitchen/commit/bc2eb738689a5562cce9d98a307f4947509d53fd))
* **operator:** fill a request's trace id from the edge ([#99](https://github.com/Bermos/Kitchen/issues/99)) ([8023ba5](https://github.com/Bermos/Kitchen/commit/8023ba5ad54cb39bd26f086f177d4517041e2c6c))
* **operator:** give the telemetry store OpenTelemetry's shape ([d24a85d](https://github.com/Bermos/Kitchen/commit/d24a85d3b91e72767daa46598097b46e7db1ebd0))
* **operator:** report deploy status back on the commit ([0f9983c](https://github.com/Bermos/Kitchen/commit/0f9983cf10e1e337d9dfdebbd5b31b68c23083fc))


### Bug fixes

* **operator:** name the pod a sample is about, not the one that sent it ([8ee5b61](https://github.com/Bermos/Kitchen/commit/8ee5b61e2a65329a3eca7d0fe3f2017ef44fee6f))


### Refactoring

* **operator:** drop the RBAC the kubelet sampling needed ([1cc1998](https://github.com/Bermos/Kitchen/commit/1cc1998774156347cbabb26bad1db6e04c7318d4))
* **operator:** hand telemetry ingest to the collector ([081144e](https://github.com/Bermos/Kitchen/commit/081144e964b2ad65076b8c8af04370985bda051f))
* **operator:** let one package name each metric ([48b36c4](https://github.com/Bermos/Kitchen/commit/48b36c4d5c4f6ff04bc8e7cf4a3ae60e05c6527f))


### Documentation

* **api:** describe the arrangement that now exists ([5dcdcd9](https://github.com/Bermos/Kitchen/commit/5dcdcd91a3941d69c045fb4b17bbe1953fbc2575))
* one collector, and what stays behind it ([c8ee09a](https://github.com/Bermos/Kitchen/commit/c8ee09ac68a7d486ccdc86805749f54b1916a970))
* **operator:** say why stream is a column and not a probe ([5c368c8](https://github.com/Bermos/Kitchen/commit/5c368c82c26307743d84b9f5b4bf493c83479ce3))

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
