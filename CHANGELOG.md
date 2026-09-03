# Changelog

## [0.23.0](https://github.com/Bermos/Kitchen/compare/v0.22.0...v0.23.0) (2026-09-03)


### ⚠ BREAKING CHANGES

* **platform:** `ProcessSpec.Previews` is now `*bool`, so that an absent value can mean "the default for this type" — off for a worker and a scheduled job, on for a service — and `false` can mean false. The wire shape is unchanged and stored objects read identically; Go code reading the field directly should call `PreviewsEnabled()` instead.

### Features

* **api:** the platform's own health checks are not a project's traffic ([062f860](https://github.com/Bermos/Kitchen/commit/062f86086ba67e974ee58e805f17284753d52d12))
* **build:** a project says which stage of its Dockerfile to ship ([#296](https://github.com/Bermos/Kitchen/issues/296)) ([d9255c5](https://github.com/Bermos/Kitchen/commit/d9255c5ef9292e6a8f86cca723bc348eba51a93d))
* **operator:** an application can ask for the security posture its workloads run under ([#291](https://github.com/Bermos/Kitchen/issues/291)) ([694b569](https://github.com/Bermos/Kitchen/commit/694b569a5483ec60ad0c3cfd191560fcc65a1bba)), closes [#276](https://github.com/Bermos/Kitchen/issues/276)
* **operator:** an operator sets the ceiling one build may take ([#290](https://github.com/Bermos/Kitchen/issues/290)) ([4c9bc95](https://github.com/Bermos/Kitchen/commit/4c9bc958701cb614896ef55dd53e89c517504ab1)), closes [#278](https://github.com/Bermos/Kitchen/issues/278)
* **operator:** work that runs once per deploy, before the release takes traffic ([#297](https://github.com/Bermos/Kitchen/issues/297)) ([2eac151](https://github.com/Bermos/Kitchen/commit/2eac151e04ec72e6e12b269d63d87e55a12b7bb5))
* **platform:** a project can ship several workloads that deploy and roll back as one ([#295](https://github.com/Bermos/Kitchen/issues/295)) ([dbd45c5](https://github.com/Bermos/Kitchen/commit/dbd45c5b2a9f62ae456cb71676b046804c8572a4)), closes [#271](https://github.com/Bermos/Kitchen/issues/271)


### Bug fixes

* **build:** one meaning for a project's root directory, and it is the build root ([#292](https://github.com/Bermos/Kitchen/issues/292)) ([19b2eb2](https://github.com/Bermos/Kitchen/commit/19b2eb23e722656282392cc51bd6414b4a1a6934)), closes [#274](https://github.com/Bermos/Kitchen/issues/274)
* **operator:** roll the workloads that read a rotated secret, and say why ([#288](https://github.com/Bermos/Kitchen/issues/288)) ([cc0a989](https://github.com/Bermos/Kitchen/commit/cc0a98914a660e3558cc0c2709e18a63e398c369)), closes [#277](https://github.com/Bermos/Kitchen/issues/277)

## [0.22.0](https://github.com/Bermos/Kitchen/compare/v0.21.0...v0.22.0) (2026-09-02)


### ⚠ BREAKING CHANGES

* **operator:** spec.scaleToZero.install, spec.databases.install and spec.databases.operatorNamespace are removed from the Kitchen object, along with status.scaleToZero and status.databases. The chart values scaleToZero.install.enabled and databases.install.enabled keep their names and their meaning — they grant the install account — and the operator seeds an Addon asking for the install, so an installation that had both set keeps installing. An installation that granted the account without setting the singleton field will now install; set spec.install false on the seeded Addon to keep it from doing so. The two roll-up conditions, ScaleToZeroReady and DatabasesReady, stay on the singleton.
* **claims:** a claim that never said what its previews get used to give them production's binding; its previews now get what the provider declares — a branch from Neon, a fresh database from CloudNativePG. previewMode: shared on the claim restores what it had. previewBranching: true still reads as the provider's own mode.

### Features

* **claims:** a workload can claim a volume and say which process mounts it ([#282](https://github.com/Bermos/Kitchen/issues/282)) ([ece6242](https://github.com/Bermos/Kitchen/commit/ece6242e7edc9af0b34bf4afbb0db04378098ce9)), closes [#267](https://github.com/Bermos/Kitchen/issues/267)
* **claims:** an application can ask the platform for a cache or a queue ([#284](https://github.com/Bermos/Kitchen/issues/284)) ([fc9677e](https://github.com/Bermos/Kitchen/commit/fc9677ee0d1d545b10f3f18a39fcbf6a8cb47b82)), closes [#265](https://github.com/Bermos/Kitchen/issues/265)
* **claims:** an application can claim a bucket to put files in ([#281](https://github.com/Bermos/Kitchen/issues/281)) ([89990be](https://github.com/Bermos/Kitchen/commit/89990be5ddc3afd04daad57131dc8885b10f1928)), closes [#266](https://github.com/Bermos/Kitchen/issues/266)
* **claims:** declare what a provider does about previews, idling and deploys ([#280](https://github.com/Bermos/Kitchen/issues/280)) ([3e9f404](https://github.com/Bermos/Kitchen/commit/3e9f404f154282e1cfdfc271d42b2891ad0fb81d))
* **claims:** durable background work has a home the platform can provide ([#283](https://github.com/Bermos/Kitchen/issues/283)) ([a18cbff](https://github.com/Bermos/Kitchen/commit/a18cbfffb2d1d57f27edb99b8685e2e330fe48ab)), closes [#268](https://github.com/Bermos/Kitchen/issues/268)
* **operator:** one engine installs the platform's dependencies, one Addon each ([#285](https://github.com/Bermos/Kitchen/issues/285)) ([c563995](https://github.com/Bermos/Kitchen/commit/c563995b693fbfdc04440c669cbb62d3eb0c9863))


### Bug fixes

* **ui:** a valkey or redis connection can be created from the dashboard ([#286](https://github.com/Bermos/Kitchen/issues/286)) ([59c445b](https://github.com/Bermos/Kitchen/commit/59c445b7dee92c32aa237e60a10f45f5f8c9af75))


### Refactoring

* **claims:** register claim types instead of branching on them ([#279](https://github.com/Bermos/Kitchen/issues/279)) ([52171b5](https://github.com/Bermos/Kitchen/commit/52171b514ed845e796db7a9d7a13b08c3ca87d44))

## [0.21.0](https://github.com/Bermos/Kitchen/compare/v0.20.0...v0.21.0) (2026-08-31)


### Features

* **api:** read a commit as a subject and a body, and let the dashboard open it ([#260](https://github.com/Bermos/Kitchen/issues/260)) ([3bbf4c8](https://github.com/Bermos/Kitchen/commit/3bbf4c8b625cfbf65eec9d12e9427be4e20998aa))
* **operator:** let a worker say two of it must never run at once ([#258](https://github.com/Bermos/Kitchen/issues/258)) ([9e388a0](https://github.com/Bermos/Kitchen/commit/9e388a0e4e6835d83fa2e596fae800a9051d5984)), closes [#250](https://github.com/Bermos/Kitchen/issues/250)


### Bug fixes

* **operator:** keep workers and cron pods out of the environment's Service ([#257](https://github.com/Bermos/Kitchen/issues/257)) ([eb12e88](https://github.com/Bermos/Kitchen/commit/eb12e88f9b0a7abb04ccb12b7aebdbec79323d00)), closes [#256](https://github.com/Bermos/Kitchen/issues/256)
* **operator:** read who installed a dependency from the job, not a lost record ([#259](https://github.com/Bermos/Kitchen/issues/259)) ([c0f090d](https://github.com/Bermos/Kitchen/commit/c0f090d2a4f87acd5b56daa956f3049e0c376794)), closes [#244](https://github.com/Bermos/Kitchen/issues/244)

## [0.20.0](https://github.com/Bermos/Kitchen/compare/v0.19.0...v0.20.0) (2026-08-30)


### Features

* **api:** report the kitchen.json a build read ([9471a05](https://github.com/Bermos/Kitchen/commit/9471a05ff0827d53c8c6304b84fe9827ecaa095f))
* **cli:** kitchen config, for the file before it is pushed ([8437807](https://github.com/Bermos/Kitchen/commit/843780700dbb4bf41e8d7c8700d2738d5667b11e))
* **operator:** build a commit the way its own kitchen.json asks ([0b07532](https://github.com/Bermos/Kitchen/commit/0b07532e4b672ea4e0026f052b82dd52912ec23e))
* **ui:** say which settings the repository has taken over ([9e31d56](https://github.com/Bermos/Kitchen/commit/9e31d56a38a5c90f0485ed65a04e65c3faa6836b))


### Bug fixes

* **cli:** print the flag forms the CLI actually accepts ([5c01fc5](https://github.com/Bermos/Kitchen/commit/5c01fc5f5c1aeaf73ca3613cab2ee9eb1ffdb84e))
* **operator:** ask again when a project's repository could not be read ([#249](https://github.com/Bermos/Kitchen/issues/249)) ([b0c447e](https://github.com/Bermos/Kitchen/commit/b0c447ea4a23be210349aca90f4bf6662ae63e33)), closes [#248](https://github.com/Bermos/Kitchen/issues/248)


### Refactoring

* **api:** give the shapes a project's settings arrive in one home ([f7e76b4](https://github.com/Bermos/Kitchen/commit/f7e76b43a4c200cbcc826ecae61f5ff08fee8c10))


### Documentation

* list kitchen.json once, in the section for the people who write it ([011c730](https://github.com/Bermos/Kitchen/commit/011c73027d237cc24f64c285e2c75cec8cad181c))
* publish the machine-readable surface as something a machine can find ([a26e02a](https://github.com/Bermos/Kitchen/commit/a26e02a0ccb402ceb3594b35c216e9c9e83c4e93))
* split the README's index by which audience each page is for ([9d85c5b](https://github.com/Bermos/Kitchen/commit/9d85c5ba197cefd13ecd39f78126a69fba201084))
* write down kitchen.json, and what a repository may not decide ([241d46a](https://github.com/Bermos/Kitchen/commit/241d46ab3b8c6dd50056160361b0b129d7d5e036))
* write the guide for the person deploying an application ([b5a2bd3](https://github.com/Bermos/Kitchen/commit/b5a2bd33dc54a8f15350cb4e1a99a685ba64467e))

## [0.19.0](https://github.com/Bermos/Kitchen/compare/v0.18.0...v0.19.0) (2026-08-30)


### Features

* **api:** let a project hold a credential the platform did not mint ([00d4d2c](https://github.com/Bermos/Kitchen/commit/00d4d2c2940b57ff20917eaccf1df8bb8ff228d3)), closes [#235](https://github.com/Bermos/Kitchen/issues/235)
* **cli:** kitchen secret, for the credentials the platform did not mint ([a5a44e6](https://github.com/Bermos/Kitchen/commit/a5a44e62141b976e2587efad06771b65af0b65f0)), closes [#235](https://github.com/Bermos/Kitchen/issues/235)
* **operator:** let a project declare its workload a singleton ([7edbf29](https://github.com/Bermos/Kitchen/commit/7edbf2970be2d03aeef51ab0857fbf899cf0263b)), closes [#239](https://github.com/Bermos/Kitchen/issues/239)
* **operator:** let a project say its workload is not request-driven ([839987d](https://github.com/Bermos/Kitchen/commit/839987de1ce84c9c07ce8cefa2a4fc7bfcba90a4)), closes [#240](https://github.com/Bermos/Kitchen/issues/240)
* **operator:** provision Postgres into the cluster with CloudNativePG ([17204bb](https://github.com/Bermos/Kitchen/commit/17204bb69a8cbef3d0e8b518cb737bf27e8cb62f))
* **operator:** start the application container with a command and arguments ([1243478](https://github.com/Bermos/Kitchen/commit/1243478b35d87428565e2ceca02a35a51dcb9270)), closes [#237](https://github.com/Bermos/Kitchen/issues/237)
* **ui:** a project's own secrets, beside its variables ([cfc2144](https://github.com/Bermos/Kitchen/commit/cfc2144c06ae773f44d13fcecdb861e167df5302)), closes [#235](https://github.com/Bermos/Kitchen/issues/235)
* **ui:** ask for the Postgres a project actually needs ([5698915](https://github.com/Bermos/Kitchen/commit/56989153c1d983e886403fe26e5b76ccdc14a36c))


### Bug fixes

* **api:** a database this platform runs is not a third party ([01470d4](https://github.com/Bermos/Kitchen/commit/01470d46c0431fef52a354a527fdc7c48f2ab122))
* **cli:** put one JSON document on stdout when "kitchen api" is refused ([#233](https://github.com/Bermos/Kitchen/issues/233)) ([63bbf6b](https://github.com/Bermos/Kitchen/commit/63bbf6bef59a7f29c3e81c74327b58a4f6e66551))
* **operator:** a preview database that is coming up has not failed ([a8868a9](https://github.com/Bermos/Kitchen/commit/a8868a9214fbadd611f5ebb63767ffbedd0a0950))
* **operator:** probe application containers before serving traffic from them ([b84f69b](https://github.com/Bermos/Kitchen/commit/b84f69bab7bfcb4feb64753e29dbfb9e78c8206a)), closes [#236](https://github.com/Bermos/Kitchen/issues/236)
* **operator:** two long claim names must not land on one database ([12da526](https://github.com/Bermos/Kitchen/commit/12da526808253158893ed6bd55645fd62a6c646f))


### Documentation

* **operator:** say what ErrNotReady and Region mean on the provider contract ([4739e4e](https://github.com/Bermos/Kitchen/commit/4739e4edb2c745e724d6387439a6acbb1ce8cf31))
* write down the self-hosted database and what it can be asked for ([ed7eee0](https://github.com/Bermos/Kitchen/commit/ed7eee05a00a87276101dd5930f78169e1aaeed4))

## [0.18.0](https://github.com/Bermos/Kitchen/compare/v0.17.0...v0.18.0) (2026-08-26)


### Features

* **signals:** compare published names against the platform's public address ([#228](https://github.com/Bermos/Kitchen/issues/228)) ([cb7d02f](https://github.com/Bermos/Kitchen/commit/cb7d02f3218f39d914c3d7e7e2944c415b519798))


### Bug fixes

* **ui:** one design guide, and the screens that were not following it ([#230](https://github.com/Bermos/Kitchen/issues/230)) ([a2a310c](https://github.com/Bermos/Kitchen/commit/a2a310ceeca95d1c5c53b4b62cc896865aa67bda))

## [0.17.0](https://github.com/Bermos/Kitchen/compare/v0.16.1...v0.17.0) (2026-08-26)


### Features

* **ui:** give an account a screen to manage itself ([509e120](https://github.com/Bermos/Kitchen/commit/509e120efa0d98fd8dc3f952f65c8358c26b8543))


### Bug fixes

* **api:** resolve a repository's default branch when detect is given no ref ([#219](https://github.com/Bermos/Kitchen/issues/219)) ([e77c4e3](https://github.com/Bermos/Kitchen/commit/e77c4e320813a93fa619aa7c36c0787ead9d9bbb))
* **api:** stop reporting a repository it cannot read as a missing root directory ([#221](https://github.com/Bermos/Kitchen/issues/221)) ([e49f120](https://github.com/Bermos/Kitchen/commit/e49f12058d3f74e5e12dd07fcc08cd0ae8ded5d8)), closes [#205](https://github.com/Bermos/Kitchen/issues/205)
* **auth:** trust the dashboard's origin, and drop the session freshness window ([f430766](https://github.com/Bermos/Kitchen/commit/f430766083fbf16c9317c5084d0ce003ae17ac2c))
* **operator:** let a project turn previews off ([#218](https://github.com/Bermos/Kitchen/issues/218)) ([b1ab3d3](https://github.com/Bermos/Kitchen/commit/b1ab3d3a0e0b3bdcd421a70390fd6eea7fe55de6))
* **ui:** let the settings page use the width it has ([c473189](https://github.com/Bermos/Kitchen/commit/c47318913279d226d6597ec49e14c7f66fb8af8d))


### Documentation

* **auth:** say what account management is, and what it is not ([0089452](https://github.com/Bermos/Kitchen/commit/008945213750ac627f295014f2fa119f8d932790))


### Build and dependencies

* run the linter under the toolchain go.mod names ([#227](https://github.com/Bermos/Kitchen/issues/227)) ([4a392d8](https://github.com/Bermos/Kitchen/commit/4a392d869533bc9fce7887c433d4a3139bb04901)), closes [#225](https://github.com/Bermos/Kitchen/issues/225)

## [0.16.1](https://github.com/Bermos/Kitchen/compare/v0.16.0...v0.16.1) (2026-08-26)


### Bug fixes

* **api:** refuse project creation to a CI key ([#215](https://github.com/Bermos/Kitchen/issues/215)) ([baa4259](https://github.com/Bermos/Kitchen/commit/baa42593ffc1ad045a6ce14ecb952e96a2dda685)), closes [#203](https://github.com/Bermos/Kitchen/issues/203)
* **chart:** give the collector a startup probe ([#196](https://github.com/Bermos/Kitchen/issues/196)) ([7e003bb](https://github.com/Bermos/Kitchen/commit/7e003bb449c6a169a462954188fd95dd0a9e03c2))
* **chart:** let the bundled registry accept the manifests builds push ([#210](https://github.com/Bermos/Kitchen/issues/210)) ([9b642f4](https://github.com/Bermos/Kitchen/commit/9b642f42f0f98981fcddaec126f45366658df4c8))
* **operator:** end a build whose job can never create a pod, and say why ([#213](https://github.com/Bermos/Kitchen/issues/213)) ([ce0ee74](https://github.com/Bermos/Kitchen/commit/ce0ee744dc7cfd90146d72ee58a5ab424834dbe6)), closes [#202](https://github.com/Bermos/Kitchen/issues/202)
* **operator:** give a pull request opened after its push the preview it is owed ([#216](https://github.com/Bermos/Kitchen/issues/216)) ([d77d309](https://github.com/Bermos/Kitchen/commit/d77d30969d6e40cb7481aac65d0d6d3f66ea3f0a)), closes [#201](https://github.com/Bermos/Kitchen/issues/201)
* **operator:** label application namespaces with a Pod Security level ([#212](https://github.com/Bermos/Kitchen/issues/212)) ([c93b1bb](https://github.com/Bermos/Kitchen/commit/c93b1bb57224aea3679f36656fa11dba880ce6aa)), closes [#199](https://github.com/Bermos/Kitchen/issues/199)
* **operator:** reconcile a project when its builds and environments change ([#214](https://github.com/Bermos/Kitchen/issues/214)) ([4bd1935](https://github.com/Bermos/Kitchen/commit/4bd1935830945f09c6811debf39723d14b30af2e)), closes [#204](https://github.com/Bermos/Kitchen/issues/204)
* **operator:** stop Dockerfile builds dying over an image they pushed ([#211](https://github.com/Bermos/Kitchen/issues/211)) ([95b8f4d](https://github.com/Bermos/Kitchen/commit/95b8f4d14789fa4cf832edde9423704a3096aee6)), closes [#200](https://github.com/Bermos/Kitchen/issues/200)

## [0.16.0](https://github.com/Bermos/Kitchen/compare/v0.15.0...v0.16.0) (2026-08-25)


### Features

* **api:** answer why a build failed, and tail it off the pod while there is one ([5be044a](https://github.com/Bermos/Kitchen/commit/5be044acb0ff50244ea412764225523971abfb09))
* **cli:** report the release a "go install" built ([e820ce6](https://github.com/Bermos/Kitchen/commit/e820ce6ec7c4f836d1deb0fb99f15605045d23e7))
* **cli:** say why a build failed ([45f3614](https://github.com/Bermos/Kitchen/commit/45f3614bd3314a66e89df4aff8253f92a860b38d))
* **operator:** say which container failed a build, and what it printed ([9e552b5](https://github.com/Bermos/Kitchen/commit/9e552b5e261623a85ec7ed061241347deb4161f0))
* **ui:** make a failed build say why, and give the tables room ([cd2707d](https://github.com/Bermos/Kitchen/commit/cd2707daff7fc9e589b7dbfe2a23b6f89a03ac27))
* **ui:** move the settings screen under the Platform section ([#191](https://github.com/Bermos/Kitchen/issues/191)) ([872b829](https://github.com/Bermos/Kitchen/commit/872b829b8c05a1895a8290de1275c12f0d4fdf86))
* **ui:** say why the last build failed, on the overview ([b97c1a5](https://github.com/Bermos/Kitchen/commit/b97c1a506d7a5704e5c85cc5ad7a5af3e93b6d49))


### Bug fixes

* **api:** report the container that failed, not the first one that spoke ([61b1106](https://github.com/Bermos/Kitchen/commit/61b1106d4c301afee2f2421ea658727153357982))
* **ui:** say node CPU and memory in units a person reads ([#194](https://github.com/Bermos/Kitchen/issues/194)) ([3870065](https://github.com/Bermos/Kitchen/commit/38700657ef8d7881e8641b28de37eec85a3810d5))


### Documentation

* **cli:** say how to install the CLI, and that no binary is published ([e505962](https://github.com/Bermos/Kitchen/commit/e5059620b0be97b19c16013053467edb3b302cc1))

## [0.15.0](https://github.com/Bermos/Kitchen/compare/v0.14.0...v0.15.0) (2026-08-25)


### Features

* **api:** say what a move between two releases would change ([36ac481](https://github.com/Bermos/Kitchen/commit/36ac4813ee18fcccd975821fd6155a398809afd9)), closes [#181](https://github.com/Bermos/Kitchen/issues/181)
* **cli:** show what a rollback changes before asking ([039e0af](https://github.com/Bermos/Kitchen/commit/039e0affe2ac11043b2879a074dc164e4a82f5b9)), closes [#181](https://github.com/Bermos/Kitchen/issues/181)
* **operator:** cron jobs and background workers per project ([#189](https://github.com/Bermos/Kitchen/issues/189)) ([b4fc869](https://github.com/Bermos/Kitchen/commit/b4fc869d90fecf497aee35c7e1cc74686347d561)), closes [#78](https://github.com/Bermos/Kitchen/issues/78)
* **ui:** make rollback pick, review the diff, then verify ([1a5a7ae](https://github.com/Bermos/Kitchen/commit/1a5a7ae35168b18f179e51fd4be3a08d5139805b)), closes [#181](https://github.com/Bermos/Kitchen/issues/181)


### Documentation

* the rollback diff, on the API and the CLI pages ([04744b3](https://github.com/Bermos/Kitchen/commit/04744b386d26ba019662101027e5bfa70b4cfcdd)), closes [#181](https://github.com/Bermos/Kitchen/issues/181)

## [0.14.0](https://github.com/Bermos/Kitchen/compare/v0.13.0...v0.14.0) (2026-08-24)


### Features

* **api:** answer what is deployed and no longer compliant ([b21bdba](https://github.com/Bermos/Kitchen/commit/b21bdba4f32bdda5dd41e89e6793826e1c657301))
* **api:** answer what supports a critical function, and what breaks without it ([a966752](https://github.com/Bermos/Kitchen/commit/a966752fcde1878d755f0e9c0872ff7e42d7a4cf))
* **api:** classify privileged transitions in the audit log ([73befde](https://github.com/Bermos/Kitchen/commit/73befde20d34d829248030f1cc5fe1c2c79b82c7)), closes [#139](https://github.com/Bermos/Kitchen/issues/139)
* **api:** export a project's audit pack ([a9a6cca](https://github.com/Bermos/Kitchen/commit/a9a6cca113140fc1fe8938c041ec085479b27fac))
* **api:** ingest OpenVEX documents as signed evidence on the artifact ([2b8a5c7](https://github.com/Bermos/Kitchen/commit/2b8a5c7d8f3d6e982961bcae0e61b1b0d034f2c6)), closes [#135](https://github.com/Bermos/Kitchen/issues/135)
* **api:** read and change the platform's retention model ([b55b671](https://github.com/Bermos/Kitchen/commit/b55b6711b9e0bdd94e761c5d70f521dea8cf89a8))
* **api:** serve a platform update's helm output ([441a3f3](https://github.com/Bermos/Kitchen/commit/441a3f38656df0ace5c92c53fa7386c16dc9e1af))
* **api:** the access recertification and identity routes, and kitchen access ([d29e9a0](https://github.com/Bermos/Kitchen/commit/d29e9a091fcf2abebc94ae4b10dfed70aa183751)), closes [#139](https://github.com/Bermos/Kitchen/issues/139)
* **cli:** read and submit exploitability assertions from a terminal ([65bc799](https://github.com/Bermos/Kitchen/commit/65bc799997a8fc121cceb0715d105ac7d8598cc6)), closes [#135](https://github.com/Bermos/Kitchen/issues/135)
* **cli:** read the criticality map and the reverse query from a terminal ([055fb99](https://github.com/Bermos/Kitchen/commit/055fb99554123e391f7204aa21b8d069af724883))
* **cli:** read the platform's retention model from a terminal ([69700cd](https://github.com/Bermos/Kitchen/commit/69700cd2f58b5a0d2c084ee2e94b4453bffe5c0d))
* **cli:** take a project's audit pack from a terminal ([7f589df](https://github.com/Bermos/Kitchen/commit/7f589df88acdeca5bfca326254d550618ee71dcb))
* **operator:** access recertification cycles, orphan survey and out-of-band detection ([534abdc](https://github.com/Bermos/Kitchen/commit/534abdc4eb3b93c77d0fed2260c01165802ff65e)), closes [#139](https://github.com/Bermos/Kitchen/issues/139)
* **operator:** carry criticality and disruption tolerance on projects and environments ([8ae1010](https://github.com/Bermos/Kitchen/commit/8ae1010696f6ed743bf510f807b377428f7f17dd))
* **operator:** enforce retention per class in the telemetry store ([b2a79d8](https://github.com/Bermos/Kitchen/commit/b2a79d85e7fbb1e51a3312b9ec5f1975c47e32e6))
* **operator:** make a declared recovery objective the threshold it fires against ([1197be7](https://github.com/Bermos/Kitchen/commit/1197be7e97e3fbe22c5cbdda2d946373f7ac1104))
* **operator:** measure node clock drift and report it as a component ([10214ff](https://github.com/Bermos/Kitchen/commit/10214ff074669e0dd024a3491380f8f94e4f4537))
* **operator:** one retention model for every class the platform keeps ([d021500](https://github.com/Bermos/Kitchen/commit/d02150014b8815f775a2b7dc0e782b9b06bd1e43))
* **operator:** re-evaluate every deployed release on a schedule ([0594ece](https://github.com/Bermos/Kitchen/commit/0594ecea344bd6d14ccde7a490bbd050f6e26720))
* **operator:** record what retention expiry removed, once a day ([dece991](https://github.com/Bermos/Kitchen/commit/dece99199f37b8ccb37f7bfa9dcb3724d418c5d9))
* **operator:** report what a self-update is actually doing ([20a4a8c](https://github.com/Bermos/Kitchen/commit/20a4a8c3c8838f6620336a7da6687cdc7798996f))
* **policy:** consult ingested OpenVEX statements when judging findings ([a1b0ceb](https://github.com/Bermos/Kitchen/commit/a1b0ceba7dad80e7c25401eb3d12c8a49ecf3af9)), closes [#135](https://github.com/Bermos/Kitchen/issues/135)
* **policy:** materialize the criticality the input has been reserving ([961e6e3](https://github.com/Bermos/Kitchen/commit/961e6e3fbf233b81a51e0d5868d6506d132ea751))
* **store:** read the signed records back, filtered and whole ([71ee584](https://github.com/Bermos/Kitchen/commit/71ee584f0cac9838f1133f8ed4a0d00f8fe444c7))
* **ui:** an auditor presses a button ([5bba46a](https://github.com/Bermos/Kitchen/commit/5bba46a888436a5a177ef2d842335c4b3ee9d179))
* **ui:** carry the criticality map, the reverse query and the boundary on the screen ([9e6e36e](https://github.com/Bermos/Kitchen/commit/9e6e36e50a339bff7ea86adec5337db8ea7baa05))
* **ui:** file an exploitability assertion from the build screen ([3580fd9](https://github.com/Bermos/Kitchen/commit/3580fd961ad5677aa14f86a1417c3f0b15c3cdec))
* **ui:** follow a platform upgrade while it happens ([ee0461c](https://github.com/Bermos/Kitchen/commit/ee0461c1a37e4816adec030a8a56874720d5ad63))
* **ui:** show every finding beside the assertion that covers it ([0979a9b](https://github.com/Bermos/Kitchen/commit/0979a9bf19687920702d81f3a4019b1b73812a6c)), closes [#135](https://github.com/Bermos/Kitchen/issues/135)
* **ui:** show what each class is kept for and how far back it goes ([48a4613](https://github.com/Bermos/Kitchen/commit/48a4613ca99bd64178591517fe02a104d564d903))
* **ui:** the access recertification panel and a privileged-only audit filter ([70949de](https://github.com/Bermos/Kitchen/commit/70949de40f291abf90938a86f97a6274c0258795)), closes [#139](https://github.com/Bermos/Kitchen/issues/139)


### Bug fixes

* **api:** answer one row per finding rather than one per rescan ([c78d384](https://github.com/Bermos/Kitchen/commit/c78d384314a7ef2b03bdc6baa6d4e183458fb7c3))
* **api:** keep a VEX document's submitter, and attest it under its own context ([59d5f04](https://github.com/Bermos/Kitchen/commit/59d5f0428b77c90fbd927e49be8a32f4033b25cc))
* **api:** make a drift row say what it is actually reporting ([83489c3](https://github.com/Bermos/Kitchen/commit/83489c3b13f8fdefe993f14a81230b5e9520d839))
* **api:** put the change log and the signed records in the list they belong to ([a10cdc5](https://github.com/Bermos/Kitchen/commit/a10cdc50a89cbf1c0d385718eb160274730bd79a))
* **api:** retry the snapshot write a reconcile can race, and pin the detection end to end ([94ac02a](https://github.com/Bermos/Kitchen/commit/94ac02a08b4699a212382e233f07cb6ecfed1bc4)), closes [#139](https://github.com/Bermos/Kitchen/issues/139)
* **operator:** refuse an auto-rollback for a release the grant never let out ([c6f4cb2](https://github.com/Bermos/Kitchen/commit/c6f4cb26adf24abff0c8751075cc13062e7920de))
* **operator:** stop a stuck pair and a finished job stalling the rescan sweep ([3c0b633](https://github.com/Bermos/Kitchen/commit/3c0b63385db6953403267ea7d90f3af225ffc69f))
* **policy:** judge an artifact on its newest vulnerability scan alone ([6fde6b7](https://github.com/Bermos/Kitchen/commit/6fde6b7f1235abf2485e445842eb0a710595e11a))
* **policy:** match vexTrustedAuthors case-insensitively, as the platform's list does ([c13419f](https://github.com/Bermos/Kitchen/commit/c13419fcb023b3568dcd13178b17ca9dd4313a17))


### Refactoring

* **api:** lift the build and runtime half of a project patch out of the handler ([827448a](https://github.com/Bermos/Kitchen/commit/827448ad044046edbfd70a32f611dc1f5762db59))
* **operator:** drop the scan report fields nothing reads ([923beb0](https://github.com/Bermos/Kitchen/commit/923beb0652962fa2f5b370e69f889f293dd6e110))
* **operator:** lift the promotion bar check into one shared evaluator ([218e017](https://github.com/Bermos/Kitchen/commit/218e017f5ecfbc9ca3109badcd461ec014e14334))
* **operator:** narrow the lease informer to the node-lease namespace ([84f8945](https://github.com/Bermos/Kitchen/commit/84f8945dd879a4367a5abf73f430824602781f70))
* **operator:** put the retention sweep's actor with the other actors ([06c079b](https://github.com/Bermos/Kitchen/commit/06c079b531f9ec4bd44e193aa5ce268034e5ea31))
* **operator:** read the retention model where the old knob was read ([b4bccd5](https://github.com/Bermos/Kitchen/commit/b4bccd5c0007ff53b5143a1af261cab8a95f161f))
* **ui:** stop the pack's time window shadowing the browser's ([c348595](https://github.com/Bermos/Kitchen/commit/c348595c8daf030ded914a1b76584ca7a53c32be))


### Documentation

* **api:** the audit pack, field by field and mapped to the requirement each satisfies ([7fc7204](https://github.com/Bermos/Kitchen/commit/7fc72043b6228b3fce5382e846afcb3343aa5af7))
* **cli:** say what kitchen drift does ([8bc8d9c](https://github.com/Bermos/Kitchen/commit/8bc8d9ce91beb1ec89f459c7094aad6d35800b37))
* **cli:** say what kitchen vex does and who may run it ([24963c3](https://github.com/Bermos/Kitchen/commit/24963c352c7ba7f3b60349f84b820eeb99b12f05)), closes [#135](https://github.com/Bermos/Kitchen/issues/135)
* **compliance:** access, privilege, and the cluster-admin residual risk ([5e5f3dd](https://github.com/Bermos/Kitchen/commit/5e5f3dd2516e96c6080de296140961241df03c0e)), closes [#139](https://github.com/Bermos/Kitchen/issues/139)
* **compliance:** point the retention comments at the section that exists ([300180e](https://github.com/Bermos/Kitchen/commit/300180ec8f85da305b39ff031d34071dfbe8eaa6))
* **compliance:** the requirement mapping, phase six of [#144](https://github.com/Bermos/Kitchen/issues/144) ([#185](https://github.com/Bermos/Kitchen/issues/185)) ([3e75a95](https://github.com/Bermos/Kitchen/commit/3e75a95508e9f27a4fd03b94bdf6d547a366ea91))
* **compliance:** write down retention, immutability and the clock ([119344b](https://github.com/Bermos/Kitchen/commit/119344bf0f7ee98c62474cd95402e678917a6cac))
* **compliance:** write down the audit pack, and close phase 5 ([e63bbb5](https://github.com/Bermos/Kitchen/commit/e63bbb57af25231a76b23a5b6c79d87ebcec5836))
* **compliance:** write down the continuous re-evaluation pass ([ff3e65f](https://github.com/Bermos/Kitchen/commit/ff3e65f626f2023bf16e05ff3753e464d3e43d18))
* **compliance:** write down what a vulnerability finding means here ([28583b7](https://github.com/Bermos/Kitchen/commit/28583b7ec802df0908b48f4bb141031dea24cb86)), closes [#135](https://github.com/Bermos/Kitchen/issues/135)
* **compliance:** write down what Kitchen may and may not say about criticality ([f7c4cfc](https://github.com/Bermos/Kitchen/commit/f7c4cfc8a5fdf3ff13d0430fc7103b316fb4e69d))
* **crds:** show the retention block and the clock-sync measurement ([563b89c](https://github.com/Bermos/Kitchen/commit/563b89ccdfcda29434a55ceafc780c4ee838ee0e))
* **observability:** say how retention became a model, and how the clock is checked ([3d350c6](https://github.com/Bermos/Kitchen/commit/3d350c6f6062e099a846483728f60ea25f4b0a7b))
* **operator:** say why the scan is attached before it is judged ([bebecca](https://github.com/Bermos/Kitchen/commit/bebecca958c58c753c81d6761413d15a737356e1))
* **policy:** name where ingested VEX statements will attach ([b52e67f](https://github.com/Bermos/Kitchen/commit/b52e67fe42233a0b2019dd62af883f93c3f9db77))
* **policy:** say what an operator upgrade does to a pinned built-in bundle ([24b1344](https://github.com/Bermos/Kitchen/commit/24b1344393b21748afff679d19d88faf9d9a5ecd)), closes [#135](https://github.com/Bermos/Kitchen/issues/135)
* warn where an operator looks that an upgrade moves the built-in bundle's digest ([cb2911e](https://github.com/Bermos/Kitchen/commit/cb2911e8c5ba7ed52d4f72f166ef688b12a11823))

## [0.13.0](https://github.com/Bermos/Kitchen/compare/v0.12.0...v0.13.0) (2026-08-24)


### Features

* **git:** implement gitlab and gitea as gitSource providers ([1ef1189](https://github.com/Bermos/Kitchen/commit/1ef11890afd490a8600cba2fd5ebb1b6419a9f0e))
* **git:** report build status back to gitlab and gitea ([cd50259](https://github.com/Bermos/Kitchen/commit/cd50259eafb1ec623e211214791a551f35995770))


### Bug fixes

* **chart:** guard missing upgrade values ([f84c074](https://github.com/Bermos/Kitchen/commit/f84c074230455b598c1fc8273d3c31bbd0a24462))
* **chart:** use safe values merge for documented upgrades ([93304d0](https://github.com/Bermos/Kitchen/commit/93304d0a36f1dd009f6def3ea382c63b30969fe7))
* **gitprovider:** let gitlab and gitea read a repository ([49615f5](https://github.com/Bermos/Kitchen/commit/49615f591245473f68b6ee66d272b6b4cb54b9e6))
* **receiver:** answer the ping, and follow each provider's own vocabulary ([84c8228](https://github.com/Bermos/Kitchen/commit/84c8228e3605cf884f7791cff0f9a68574de5223))
* **ui:** say what a gitlab or gitea token actually needs ([fcf9202](https://github.com/Bermos/Kitchen/commit/fcf9202a78ae7cd8ab223bc5c7e24f29c4e1dd76))


### Refactoring

* **gitprovider:** resolve the api url in one place ([7598f34](https://github.com/Bermos/Kitchen/commit/7598f34924646e33524fa7ae5b3c0affb6e755f4))
* **gitprovider:** split the deployment record out of StatusReporter ([36cc8e6](https://github.com/Bermos/Kitchen/commit/36cc8e60a8a412923235bb8db5ea74bf69450f4e))


### Documentation

* **git:** record gitlab and gitea as landed sources ([cbb3cea](https://github.com/Bermos/Kitchen/commit/cbb3cea428092541eaeb2da09593d7873059405d))
* **git:** say that gitlab and gitea now report back ([a790256](https://github.com/Bermos/Kitchen/commit/a79025692f3628671e9723e09063c9c2d90956aa))

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
