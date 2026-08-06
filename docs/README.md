# Tài liệu

## `codebase-guide-vi.html` — Giải thích toàn bộ codebase (tiếng Việt)

Hướng dẫn đọc codebase dành cho người mới với Go và hạ tầng. Trang HTML
độc lập (không phụ thuộc mạng, không CDN), mở trực tiếp bằng trình duyệt:

```bash
open docs/codebase-guide-vi.html          # macOS
xdg-open docs/codebase-guide-vi.html      # Linux
```

Nội dung gồm 25 chương, 12 sơ đồ SVG, và ba phụ lục tra cứu:

| Phần | Chương | Nội dung |
|---|---|---|
| I · Toàn cảnh | 00–03 | Service làm gì, bản đồ một request, cây thư mục, kiến trúc 4 tầng |
| II · Mã nguồn Go | 04–11 | Proto, transport, use case, ba cuộc đua tranh, `pkg/xsql`, `pkg/dataservicex`, repository, schema |
| III · Tiến trình | 12–16 | Vòng đời `cmd/api`, config, migrations, client + load balancer, observability |
| IV · Hạ tầng | 17–21 | Docker, Helm/Kubernetes, Gateway API + TLS + NetworkPolicy, Argo CD, CI và chiến lược test |
| Phụ lục | A–C | Từ điển Go, từ điển hạ tầng, lộ trình đọc code 7 chặng |

Tài liệu trích dẫn code nguyên văn từ repository và giải thích **vì sao**
mỗi quyết định được đưa ra, không chỉ **cái gì** đang xảy ra — bám theo các
chú thích thiết kế đã có sẵn trong mã nguồn.

Trang hỗ trợ cả giao diện sáng và tối theo thiết lập hệ điều hành.
