# How to Get a VK Token

## Способ 1: Через vkhost.github.io (Самый простой)

1. Откройте https://vkhost.github.io/
2. Выберите любое приложение из списка (например, **VK Admin** или **Kate Mobile**)
3. Поставьте галочку **"Доступ в любое время"** — это даёт бессрочный токен
4. Нажмите **"Получить"**
5. Авторизуйтесь в VK и нажмите "Разрешить"
6. Скопируйте `access_token` из адресной строки (начинается с `vk1.a.`)

## Способ 2: Via OAuth URL

1. Откройте в браузере:

   ```
   https://oauth.vk.com/authorize?client_id=6287487&display=page&redirect_uri=https://oauth.vk.com/blank.html&scope=offline&response_type=token&v=5.274
   ```

2. Нажмите "Разрешить"
3. Вы будете перенаправлены на URL вида:
   ```
   https://oauth.vk.com/blank.html#access_token=vk1.a.XXXXX&expires_in=0&user_id=12345
   ```
4. Скопируйте значение `access_token` (начинается с `vk1.a.`)

## Свой App ID

1. Перейдите на https://vk.com/editapp?act=create
2. Создайте приложение типа Standalone
3. Запомните App ID
4. Замените `client_id=6287487` в URL выше на свой App ID

## Usage

```bash
# Single token
callvpn-client --link=<link> --vk-token='vk1.a.XXXXX'

# Multiple tokens (different accounts)
callvpn-client --link=<link> --vk-token='vk1.a.TOKEN1' --vk-token='vk1.a.TOKEN2'

# Via environment variable
export VK_TOKENS='vk1.a.TOKEN1,vk1.a.TOKEN2'
callvpn-client --link=<link>
```

## Shell Escaping

If using OK auth_tokens (starting with `$`), use single quotes to prevent shell expansion:
```bash
--vk-token='$xCUTeFZFtk...'
```

## Token Lifetime

- **VK access_token** (`vk1.a.*`): permanent with `offline` scope
- **OK auth_token** (`$*`): unknown lifetime, may expire — use VK tokens when possible
