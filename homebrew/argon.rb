class Argon < Formula
  desc "Git-like MongoDB branching for ML/AI workflows"
  homepage "https://github.com/argon-lab/argon"
  url "https://github.com/argon-lab/argon/archive/v1.0.0.tar.gz"
  sha256 "TBD" # Will be updated with actual release
  license "MIT"
  head "https://github.com/argon-lab/argon.git", branch: "main"

  depends_on "go" => :build

  def install
    cd "cli" do
      system "go", "build", *std_go_args(ldflags: "-s -w"), "-o", bin/"argon"
    end
  end

  test do
    system "#{bin}/argon", "--version"
    assert_match "argon version", shell_output("#{bin}/argon --version")
  end
end