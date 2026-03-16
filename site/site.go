package site

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	katex "github.com/FurqanSoftware/goldmark-katex"
)

// RepoConfig 表示 config.json 中配置的一个 Git 仓库
type RepoConfig struct {
	// URL 为远程 Git 仓库地址（必填）
	URL string `json:"url"`
	// Title 用作 hostname/{title} 中的 title，可选；若为空，则从 URL 自动推导（使用仓库名）
	Title string `json:"title,omitempty"`
	// Dir 为该仓库在当前项目中的工作目录，相对于项目根目录，可选；
	// 若为空，则自动使用 "repos/{repoName}"，其中 repoName 从 URL 推导。
	Dir string `json:"dir,omitempty"`
}

// Config 是 config.json 的整体结构
type Config struct {
	Repos []RepoConfig `json:"repos"`
}

// Post 表示一篇博客文章的元信息和内容
type Post struct {
	Title      string
	Slug       string
	Path       string // 相对输出路径，例如 "go/hello-world/index.html"
	BlogTitle  string // 来自 RepoConfig.Title，用于 hostname/{title}
	Date       time.Time
	Category   string
	Difficulty string
	Content    template.HTML
}

// BuildSite 从 config.json 中读取唯一的仓库，扫描仓库根目录下的 Markdown 文件并生成到 outputDir。
// 每个 Markdown 文件会被渲染为一篇文章，标题由文件名（不含扩展名）决定。
// 旧版本支持minicago blog仓库并递归扫描，此处简化为单仓库，不进入子目录。
func BuildSite(configPath, outputDir string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	repos, err := normalizeRepos(cfg.Repos)
	if err != nil {
		return err
	}

	// 当前只支持一个仓库，忽略多余配置
	if len(repos) == 0 {
		return fmt.Errorf("配置中未指定任何仓库")
	}
	repo := repos[0]

	// 确保仓库已经 clone / pull 到最新
	if err := syncRepo(repo); err != nil {
		return err
	}

	if err := os.RemoveAll(outputDir); err != nil {
		return fmt.Errorf("清理输出目录失败: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	// 复制静态资源（图片、HTML、CSS 等），便于后续 Markdown 中引用
	if err := copyStaticAssets(repo.Dir, outputDir); err != nil {
		return fmt.Errorf("复制静态资源失败: %w", err)
	}

	posts, err := collectPostsFromRepo(repo)
	if err != nil {
		return err
	}

	// 全站按时间倒序排列
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].Date.After(posts[j].Date)
	})

	if err := writePosts(outputDir, posts); err != nil {
		return err
	}
	if err := writeGlobalIndex(outputDir, posts, repo.Dir); err != nil {
		return err
	}
	// 个人博客功能无需生成额外索引
	if err := writeHelp(outputDir, []RepoConfig{repo}); err != nil {
		return err
	}

	return nil
}

// loadConfig 读取并解析 config.json
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败 %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败 %s: %w", path, err)
	}
	return &cfg, nil
}

// normalizeRepos 根据 URL 自动推导缺失的 Title 和 Dir
// - 若 Title 为空，则使用 repoName（从 URL 解析出的仓库名）作为 Title
// - 若 Dir 为空，则使用 "repos/{repoName}" 作为本地目录
func normalizeRepos(repos []RepoConfig) ([]RepoConfig, error) {
	out := make([]RepoConfig, 0, len(repos))
	for _, r := range repos {
		if r.URL == "" {
			return nil, fmt.Errorf("配置错误: 存在缺少 url 的仓库配置")
		}
		// 对常见 SSH 形式的 GitHub 地址做一次归一化，优先使用 HTTPS，
		// 这样 public 仓库可以直接拉取而不依赖本机 SSH key。
		r.URL = normalizeGitURL(r.URL)

		name, err := repoNameFromURL(r.URL)
		if err != nil {
			return nil, fmt.Errorf("从 URL 解析仓库名失败 %q: %w", r.URL, err)
		}
		if r.Title == "" {
			r.Title = name
		}
		if r.Dir == "" {
			r.Dir = filepath.Join("repos", name)
		}
		out = append(out, r)
	}
	return out, nil
}

// repoNameFromURL 从常见的 Git URL 中解析仓库名
// 示例：
// - https://github.com/user/repo.git -> "repo"
// - git@github.com:user/repo.git    -> "repo"
func repoNameFromURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("空 URL")
	}
	// 去掉末尾的斜杠
	raw = strings.TrimRight(raw, "/")
	// 找到最后一个 / 或 :
	i := strings.LastIndexAny(raw, "/:")
	if i == -1 || i == len(raw)-1 {
		return "", fmt.Errorf("无法从 URL 提取仓库名: %s", raw)
	}
	name := raw[i+1:]
	name = strings.TrimSuffix(name, ".git")
	if name == "" {
		return "", fmt.Errorf("解析到空仓库名: %s", raw)
	}
	return name, nil
}

// normalizeGitURL 将常见的 SSH 形式 GitHub 地址转换为 HTTPS，
// 以便在公共仓库场景下无需 SSH key 就能直接 clone/pull。
// 例如：
// - git@github.com:user/repo.git    -> https://github.com/user/repo.git
// - ssh://git@github.com/user/repo  -> https://github.com/user/repo
func normalizeGitURL(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "git@github.com:") {
		rest := strings.TrimPrefix(s, "git@github.com:")
		return "https://github.com/" + rest
	}
	if strings.HasPrefix(s, "ssh://git@github.com/") {
		rest := strings.TrimPrefix(s, "ssh://git@github.com/")
		return "https://github.com/" + rest
	}
	return raw
}

// syncRepos 根据配置对每个仓库执行 git clone / git pull
func syncRepos(repos []RepoConfig) error {
	for _, repo := range repos {
		if err := syncRepo(repo); err != nil {
			return err
		}
	}
	return nil
}

// syncRepo 确保单个仓库在本地可用：
// - 如果配置了 URL 且目录不存在：执行 git clone
// - 如果配置了 URL 且目录已存在且是 Git 仓库：执行 git pull
// - 如果未配置 URL：仅检查目录存在
func syncRepo(repo RepoConfig) error {
	info, err := os.Stat(repo.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			// 目录不存在，如果配置了 URL，则 clone
			if repo.URL == "" {
				return fmt.Errorf("本地仓库目录不存在且未配置 URL: %s", repo.Dir)
			}
			return gitClone(repo.URL, repo.Dir)
		}
		return fmt.Errorf("检查目录失败 %s: %w", repo.Dir, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("路径不是目录: %s", repo.Dir)
	}

	// 目录存在，如果配置了 URL，则尝试 pull
	if repo.URL != "" {
		if _, err := os.Stat(filepath.Join(repo.Dir, ".git")); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("目录 %s 存在但不是 Git 仓库（缺少 .git）", repo.Dir)
			}
			return fmt.Errorf("检查 .git 目录失败 %s: %w", repo.Dir, err)
		}
		return gitPull(repo.Dir)
	}

	// 未配置 URL，且目录存在，直接使用本地内容
	return nil
}

func gitClone(url, dir string) error {
	cmd := exec.Command("git", "clone", "--depth", "1", url, dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func gitPull(dir string) error {
	cmd := exec.Command("git", "-C", dir, "pull", "--ff-only")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// extractMetadataFromMarkdown 解析文件开头的 ```metadata ... ``` 块，返回解析出的字段和去掉 metadata 的内容。
func extractMetadataFromMarkdown(content string) (map[string]string, string) {
	re := regexp.MustCompile("(?s)^\\s*```metadata\\s*\\n(.*?)\\n```(?:\\r?\\n)?(.*)$")
	matches := re.FindStringSubmatch(content)
	if len(matches) != 3 {
		return nil, content
	}

	metaText := matches[1]
	rest := matches[2]

	meta := make(map[string]string)
	for _, line := range strings.Split(metaText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		value := strings.TrimSpace(parts[1])
		meta[key] = value
	}

	return meta, rest
}

func parseDateString(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02",
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析日期: %s", s)
}

// collectPostsFromRepo 只扫描仓库根目录下的 Markdown 文件，标题使用文件名
// (不递归)。在转换内容之前会修正 Markdown 中的相对链接，以便
// 访问构建后静态目录中的图像/HTML/其他资源。
func collectPostsFromRepo(repo RepoConfig) ([]Post, error) {
	var posts []Post
	// 使用带有 GFM、表格、链接、图片和数学支持的 Markdown 解析器
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Linkify,
			extension.Strikethrough,
			extension.Typographer,
			extension.Table,
			extension.DefinitionList,
			extension.Footnote,
			&katex.Extender{}, // math support via KaTeX
		// 更多扩展可在此追加
		// 图片、链接等在 GFM 中已支持
		// 如果需要自动编号等，可加入其他扩展
		// 上面的包都在 imports 中声明
		),
	)

	root := repo.Dir
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".md" {
			continue
		}

		path := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取文件失败 %s: %w", path, err)
		}

		title := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		body := string(data)

		metadata, parsedBody := extractMetadataFromMarkdown(body)
		if parsedBody != "" {
			body = parsedBody
		}

		category := ""
		difficulty := ""
		if metadata != nil {
			if v := strings.TrimSpace(metadata["title"]); v != "" {
				title = v
			}
			if v := strings.TrimSpace(metadata["category"]); v != "" {
				category = v
			}
			if v := strings.TrimSpace(metadata["difficulty"]); v != "" {
				difficulty = v
			}
		}

		// 在渲染之前修正 Markdown 中的相对链接，使它们在输出目录中
		// 能够通过以 `/` 开头的绝对路径访问。
		body = fixRelativePathsInMarkdown(body)

		var buf bytes.Buffer
		if err := md.Convert([]byte(body), &buf); err != nil {
			return nil, fmt.Errorf("Markdown 转 HTML 失败 %s: %w", path, err)
		}
		// 渲染完成后，进一步处理生成的 html，针对可能漏掉的情况
		html := rewriteRelativeLinksInHTML(buf.String())
		buf.Reset()
		buf.WriteString(html)

		slug := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		outPath := filepath.ToSlash(filepath.Join(slug, "index.html"))

		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}

		postDate := info.ModTime()
		if metadata != nil {
			if d, found := metadata["date"]; found {
				if parsed, perr := parseDateString(d); perr == nil {
					postDate = parsed
				}
			}
		}

		post := Post{
			Title:      title,
			Slug:       slug,
			Path:       outPath,
			BlogTitle:  "", // 现在仅一个仓库，无需博客标题
			Date:       postDate,
			Category:   category,
			Difficulty: difficulty,
			Content:    template.HTML(buf.String()),
		}
		posts = append(posts, post)
	}

	// 按时间倒序排列
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].Date.After(posts[j].Date)
	})

	return posts, nil
}

// writePosts 将每篇文章渲染到独立页面
func writePosts(outputDir string, posts []Post) error {
	tmpl, err := loadTemplate("post")
	if err != nil {
		return err
	}

	for _, p := range posts {
		outPath := filepath.Join(outputDir, filepath.FromSlash(p.Path))
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}

		f, err := os.Create(outPath)
		if err != nil {
			return err
		}

		if err := tmpl.Execute(f, p); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
	}
	return nil
}

// renderMarkdownToHTML 将 Markdown 内容转换为 HTML
func renderMarkdownToHTML(markdown string) (template.HTML, error) {
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Linkify,
			extension.Strikethrough,
			extension.Typographer,
			extension.Table,
			extension.DefinitionList,
			extension.Footnote,
			&katex.Extender{},
		),
	)

	var buf bytes.Buffer
	if err := md.Convert([]byte(markdown), &buf); err != nil {
		return "", err
	}
	return template.HTML(rewriteRelativeLinksInHTML(buf.String())), nil
}

// writeGlobalIndex 生成全站首页（根目录 index.html 和 /index）
func writeGlobalIndex(outputDir string, posts []Post, repoDir string) error {
	tmpl, err := loadTemplate("index")
	if err != nil {
		return err
	}

	latestCount := 3
	if len(posts) < latestCount {
		latestCount = len(posts)
	}
	latestPosts := posts[:latestCount]

	categories := make(map[string][]Post)
	for _, p := range posts {
		cat := strings.TrimSpace(p.Category)
		if cat == "" {
			cat = "未分类"
		}
		categories[cat] = append(categories[cat], p)
	}

	readmePaths := []string{"README.md", "readme.md"}
	readmeHTML := template.HTML("")
	for _, rp := range readmePaths {
		readmePath := filepath.Join(repoDir, rp)
		if _, err := os.Stat(readmePath); err == nil {
			content, err := os.ReadFile(readmePath)
			if err == nil {
				r, err := renderMarkdownToHTML(string(content))
				if err == nil {
					readmeHTML = r
					break
				}
			}
		}
	}

	data := struct {
		LatestPosts []Post
		ReadmeHTML  template.HTML
		Categories  map[string][]Post
	}{
		LatestPosts: latestPosts,
		ReadmeHTML:  readmeHTML,
		Categories:  categories,
	}

	// 1) 根目录 index.html（/）
	rootIndex := filepath.Join(outputDir, "index.html")
	if err := writeTemplateFile(rootIndex, tmpl, data); err != nil {
		return err
	}

	// 2) /index/ （hostname/index）
	indexDir := filepath.Join(outputDir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return err
	}
	indexFile := filepath.Join(indexDir, "index.html")
	return writeTemplateFile(indexFile, tmpl, data)
}

// writeBlogIndexes 为每个博客生成 hostname/{title} 首页
func writeBlogIndexes(outputDir string, postsByBlog map[string][]Post) error {
	if len(postsByBlog) == 0 {
		return nil
	}

	tmpl, err := loadTemplate("index")
	if err != nil {
		return err
	}

	for title, posts := range postsByBlog {
		dir := filepath.Join(outputDir, title)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		outPath := filepath.Join(dir, "index.html")
		data := struct {
			Posts []Post
		}{
			Posts: posts,
		}
		if err := writeTemplateFile(outPath, tmpl, data); err != nil {
			return err
		}
	}
	return nil
}

// writeHelp 生成 /help 页面
func writeHelp(outputDir string, repos []RepoConfig) error {
	tmpl, err := loadTemplate("help")
	if err != nil {
		return err
	}

	dir := filepath.Join(outputDir, "help")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(dir, "index.html")

	data := struct {
		Repos []RepoConfig
	}{
		Repos: repos,
	}

	return writeTemplateFile(outPath, tmpl, data)
}

// writeTemplateFile 是一个小工具函数，用于将模板执行结果写入文件
func writeTemplateFile(path string, tmpl *template.Template, data any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}

// fixRelativePathsInMarkdown 将 Markdown 文本中所有非绝对或协议开头的链接路径
// 统一以 `/` 前缀处理。这样用户在 Markdown 里写 `![alt](foo.png)` 或
// `[链接](page.html)` 时，生成页面中就会变为 `/foo.png` 或 `/page.html`，
// 对应于输出目录的根。如果路径包含 `./` 或 `../` 会被清理。
func fixRelativePathsInMarkdown(markdown string) string {
	re := regexp.MustCompile(`(?m)(!\[[^\]]*\]|\[[^\]]*\])\(([^)]+)\)`)
	return re.ReplaceAllStringFunc(markdown, func(m string) string {
		sub := re.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		link := sub[2]
		// 保留以 http://、https://、/、# 开头的链接不变
		if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") ||
			strings.HasPrefix(link, "/") || strings.HasPrefix(link, "#") {
			return m
		}
		clean := strings.TrimPrefix(link, "./")
		clean = strings.TrimPrefix(clean, "../")
		return strings.Replace(m, link, "/"+clean, 1)
	})
}

// rewriteRelativeLinksInHTML 对已经生成的 HTML 再做一次检查，针对
// <a> 和 <img> 等标签，如果属性值仍然是相对路径，则加上 `/` 前缀。
func rewriteRelativeLinksInHTML(html string) string {
	// 先匹配任何 src 或 href 属性
	re := regexp.MustCompile(`(?i)(?:src|href)="([^"]+)"`)
	return re.ReplaceAllStringFunc(html, func(m string) string {
		parts := re.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		path := parts[1]
		// 忽略以 http://、https://、/ 或 # 开头的路径
		if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") ||
			strings.HasPrefix(path, "/") || strings.HasPrefix(path, "#") {
			return m
		}
		clean := strings.TrimPrefix(path, "./")
		clean = strings.TrimPrefix(clean, "../")
		return strings.Replace(m, path, "/"+clean, 1)
	})
}

// copyStaticAssets 从指定仓库目录复制所有非 Markdown 文件到输出目录，
// 保留相对路径。该函数会递归遍历仓库目录，但会跳过 `.git` 目录。
func copyStaticAssets(repoDir, outputDir string) error {
	return filepath.Walk(repoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// 忽略 .git 目录
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".md" {
			// Markdown 由生成逻辑处理，不复制
			return nil
		}
		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(outputDir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return copyFile(path, dest)
	})
}

// copyFile 是一个简单的文件复制工具。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// loadTemplate 从 templates 目录加载模板
func loadTemplate(name string) (*template.Template, error) {
	base := "templates/base.html"
	page := fmt.Sprintf("templates/%s.html", name)
	return template.ParseFiles(base, page)
}
