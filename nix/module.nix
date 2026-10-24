{ config, lib, pkgs, ... }:

let
  cfg = config.services.nixhook;
in {
  options.services.nixhook = {
    enable = lib.mkEnableOption "nixhook, an HTTP endpoint that deploys this machine's flake configuration";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The nixhook package to use.";
    };

    listen = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:9099";
      description = "Address to listen on. Keep it on localhost and expose it through a tunnel or reverse proxy.";
    };

    flake = lib.mkOption {
      type = lib.types.str;
      default = "/etc/nixos";
      example = "github:owner/nixos-config";
      description = ''
        Where the configuration comes from. A local path is a git checkout
        that is updated with `git pull --ff-only` before each deploy. Anything
        else is a flake URI that nix fetches, pinned to the commit the
        requested ref resolves to.
      '';
    };

    allowedTargets = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "homeServer" ];
      description = "nixosConfigurations this machine may switch to. Empty allows any the flake defines.";
    };

    defaultTarget = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "homeServer";
      description = "Target deployed when a request does not name one.";
    };

    rollbackOnFailure = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Switch back to the previous generation when the switch or the post-switch check fails.";
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/nixhook";
      description = "Directory holding the API key and job history. The key is generated on first start.";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];

    systemd.services.nixhook = {
      description = "nixhook deploy endpoint";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];

      # Deploys run in a transient unit started from this PATH, so these are
      # the tools a deploy can use.
      path = [ config.nix.package pkgs.git pkgs.openssh config.system.build.nixos-rebuild ];
      environment.HOME = "/root";

      serviceConfig = {
        ExecStart = lib.escapeShellArgs ([
          "${cfg.package}/bin/nixhook" "serve"
          "--listen" cfg.listen
          "--flake" cfg.flake
          "--state-dir" cfg.stateDir
        ]
        ++ lib.optionals (cfg.allowedTargets != [ ]) [ "--allow" (lib.concatStringsSep "," cfg.allowedTargets) ]
        ++ lib.optionals (cfg.defaultTarget != null) [ "--default-target" cfg.defaultTarget ]
        ++ lib.optional cfg.rollbackOnFailure "--rollback");
        Restart = "always";
        RestartSec = 5;
      } // lib.optionalAttrs (cfg.stateDir == "/var/lib/nixhook") {
        StateDirectory = "nixhook";
        StateDirectoryMode = "0700";
      };
    };
  };
}
