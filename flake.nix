{
  description = "chat-agent — 带长期记忆与知识库 RAG 的 kei 聊天代理插件";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      devShells = forAllSystems (system: {
        default = (nixpkgs.legacyPackages.${system}.mkShell {
          name = "chat-agent";

          packages = with nixpkgs.legacyPackages.${system}; [
            go
            gcc
            gopls
            gotools
            golangci-lint
            git
          ];

          # 只使用 devShell 提供的 Go，禁止自动下载其它 toolchain。
          env.GOTOOLCHAIN = "local";

          shellHook = ''
            echo "chat-agent dev shell · $(go version)"
            echo "常用命令: gofmt -l . | go build ./... | go test -race ./..."
          '';
        });
      });
    };
}
