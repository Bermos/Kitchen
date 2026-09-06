// The application behind the buildpacks end-to-end case.
//
// It answers one thing and it is the whole assertion: an image that says
// `buildpacks` can only have come out of the Cloud Native Buildpacks
// lifecycle, because there is no Dockerfile in this directory for the other
// strategy to have built.
//
// `$PORT` is the platform's own variable and the only way a buildpacks-built
// image is told where to listen — every buildpack's answer to "which port does
// this process serve on" is that variable, and an application that never had a
// Dockerfile has no other way to be told.
const http = require("http");

const port = Number(process.env.PORT) || 8080;

http
  .createServer((_request, response) => {
    response.writeHead(200, { "content-type": "text/plain" });
    response.end(`buildpacks ${port}\n`);
  })
  .listen(port, () => {
    console.log(`the buildpacks fixture is listening on ${port}`);
  });
