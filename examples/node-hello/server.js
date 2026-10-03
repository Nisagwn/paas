const http = require("http");

const port = process.env.PORT || 8080;

http
  .createServer((req, res) => {
    res.setHeader("Content-Type", "text/plain");
    res.end("hello from node\n");
  })
  .listen(port, () => console.log(`listening on :${port}`));
