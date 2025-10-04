# OnleXL - Система управления поставщиками

Веб-приложение для управления базой данных поставщиков с возможностью автоматического получения данных компаний через API Checko.

## 🚀 Особенности

- 👥 **Система пользователей** - регистрация, авторизация, профили
- 🏢 **Управление поставщиками** - добавление, редактирование, поиск компаний
- 📞 **Контактная информация** - телефоны, email, сайты с отметками о звонках/письмах
- ⭐ **Избранные контакты** - маркировка важных телефонов и email
- 🔍 **Умный поиск** - по названию, ИНН, контактам с автодополнением
- 📊 **Статусы компаний** - new, contacted, interested, rejected, closed
- 📈 **История изменений** - логирование всех действий пользователей
- 📋 **Экспорт данных** - выгрузка в Excel с фильтрацией
- 🤖 **Интеграция с Checko API** - автоматическое получение данных по ИНН
- 👑 **Панель администратора** - управление компаниями, удаление записей
- 🔒 **Безопасность** - хеширование паролей, защищенные cookies
- 💾 **Автобэкапы** - ежедневные дампы PostgreSQL

## 🛠️ Технический стек

- **Backend**: Go 1.25+ с Gin Framework
- **База данных**: PostgreSQL 12+
- **Frontend**: HTML5, CSS3, JavaScript (Vanilla)
- **Шаблоны**: Go HTML Templates
- **Развертывание**: systemd + Nginx + SSL
- **API интеграция**: Checko.ru для получения данных компаний

## 📋 Требования

- Go 1.25 или выше
- PostgreSQL 12 или выше
- Nginx (для продакшена)
- API ключ Checko.ru (опционально)

## 🔧 Установка

### 1. Клонирование репозитория

```bash
git clone <repository-url>
cd onlexl
```

### 2. Установка зависимостей

```bash
go mod tidy
```

### 3. Настройка базы данных

Создайте базу данных PostgreSQL и импортируйте схему:

```bash
createdb onlexl
psql -d onlexl -f dump-onlexl-noowner.sql
```

### 4. Конфигурация

Создайте файл `.env`:

```env
# База данных
POSTGRES_USER=your_username
POSTGRES_PASSWORD=your_password
POSTGRES_HOST=localhost
POSTGRES_PORT=5432
POSTGRES_DB=onlexl
POSTGRES_SCHEMA=public

# Cookies и безопасность
COOKIE_DOMAIN=auto
COOKIE_SECURE=false

# Административный доступ
ADMIN_PASSWORD=your_admin_password

# API интеграция (опционально)
CHECKO_API_KEY=your_checko_api_key
```

### 5. Запуск приложения

#### Для разработки:
```bash
go run main.go
```

#### Для продакшена:
```bash
go build -o myapp main.go
./myapp
```

Приложение будет доступно по адресу: `http://localhost:8080`

## 🐳 Docker (альтернативный способ)

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod tidy && go build -o myapp main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates postgresql-client
WORKDIR /root/
COPY --from=builder /app/myapp .
COPY --from=builder /app/templates ./templates
COPY --from=builder /app/static ./static
CMD ["./myapp"]
```

## 📁 Структура проекта

```
onlexl/
├── main.go                 # Основной файл приложения
├── .env                    # Конфигурация (не в git)
├── go.mod                  # Go модули
├── go.sum                  # Checksums модулей
├── static/
│   └── style.css          # CSS стили
├── templates/             # HTML шаблоны
│   ├── layout.html        # Базовый шаблон
│   ├── login.html         # Страница входа
│   ├── register.html      # Регистрация
│   ├── companies.html     # Список компаний
│   ├── company.html       # Карточка компании
│   ├── addcompany.html    # Добавление компании
│   ├── delcompany.html    # Удаление компании
│   ├── export.html        # Экспорт данных
│   ├── profile.html       # Профиль пользователя
│   └── apanel.html        # Панель администратора
├── dump-*.sql             # SQL дампы
└── README.md              # Этот файл
```

## 🗄️ Схема базы данных

### Основные таблицы:

- **users** - пользователи системы
- **suppliers** - поставщики/компании
- **supplier_phones** - телефоны поставщиков
- **supplier_emails** - email адреса поставщиков
- **supplier_history** - история изменений

### Статусы компаний:
- `new` - новая компания
- `contacted` - был контакт
- `interested` - проявили интерес
- `rejected` - отказались
- `closed` - сделка закрыта

## 👥 Использование

### 1. Регистрация и вход
- Зарегистрируйтесь на `/register`
- Войдите в систему на `/login`

### 2. Работа с компаниями
- **Просмотр списка**: `/companies`
- **Поиск**: используйте фильтры и строку поиска
- **Добавление**: `/addcompany` (только для админов)
- **Редактирование**: нажмите на компанию в списке

### 3. Административные функции
- **Доступ к панели**: `/apanel`
- **Удаление компаний**: `/delcompany`
- **Добавление через Checko API**: укажите ИНН в форме добавления

### 4. Экспорт данных
- **Экспорт в Excel**: `/export`
- Доступны фильтры по пользователю, датам, статусу, компании

## 🔐 Безопасность

- Пароли хешируются с помощью bcrypt
- Защита от CSRF атак
- Безопасные HTTP-only cookies
- Поддержка HTTPS с настройкой Secure cookies
- Авторизация на уровне маршрутов
- Двухуровневая система доступа (пользователь + админ)

## 🚀 Развертывание

### Systemd сервис

Создайте файл `/etc/systemd/system/myapp.service`:

```ini
[Unit]
Description=OnleXL Web Application
After=network.target

[Service]
User=your_user
WorkingDirectory=/path/to/onlexl/
Environment="POSTGRES_USER=user"
Environment="POSTGRES_PASSWORD=password"
Environment="POSTGRES_HOST=localhost"
Environment="POSTGRES_PORT=5432"
Environment="POSTGRES_DB=onlexl"
Environment="GIN_MODE=release"
ExecStart=/path/to/onlexl/myapp
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

### Nginx конфигурация

```nginx
server {
    listen 443 ssl http2;
    server_name your-domain.com;

    ssl_certificate /path/to/certificate.crt;
    ssl_certificate_key /path/to/private.key;

    location / {
        proxy_pass http://localhost:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
    }
}
```

## 📊 API интеграция

Приложение поддерживает интеграцию с Checko.ru API для автоматического получения данных компаний:

1. Получите API ключ на [checko.ru](https://checko.ru)
2. Укажите его в переменной `CHECKO_API_KEY`
3. В форме добавления компании введите ИНН
4. Данные будут получены автоматически

## 🛠️ Разработка

### Запуск в режиме разработки:

```bash
# Установка air для hot reload
go install github.com/cosmtrek/air@latest

# Запуск с автоперезагрузкой
air
```

### Структура функций:

- `loadEnv()` - загрузка переменных окружения
- `initDB()` - инициализация БД
- `authRequired()` - middleware авторизации
- `requireAdmin()` - проверка прав администратора
- `fetchChecko()` - интеграция с Checko API

## 📈 Мониторинг

Приложение ведет подробные логи:
- Все HTTP запросы (Gin логирование)
- Ошибки базы данных
- Предупреждения о конфигурации
- Успешные операции

Автоматические бэкапы создаются ежедневно в 23:59.

## 🤝 Участие в разработке

1. Fork репозитория
2. Создайте feature branch (`git checkout -b feature/AmazingFeature`)
3. Commit изменения (`git commit -m 'Add some AmazingFeature'`)
4. Push в branch (`git push origin feature/AmazingFeature`)
5. Создайте Pull Request

## 📝 Лицензия

Этот проект лицензирован под MIT License - см. файл [LICENSE](LICENSE) для деталей.

## 📞 Поддержка

Если у вас есть вопросы или проблемы:

1. Проверьте [Issues](../../issues) на GitHub
2. Создайте новый Issue с описанием проблемы
3. Приложите логи и конфигурацию (без паролей!)

## 🔄 Обновления

Для обновления приложения:

1. Сделайте бэкап базы данных
2. Остановите сервис: `sudo systemctl stop myapp`
3. Обновите код: `git pull`
4. Пересоберите: `go build -o myapp main.go`
5. Запустите сервис: `sudo systemctl start myapp`

---

**Версия**: 1.0.0  
**Последнее обновление**: Октябрь 2025